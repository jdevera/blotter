package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jdevera/blotter/internal/store"
)

const vaultSettings = `{
  "permissions": {"allow": ["Read"]},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Write|Edit", "hooks": [
        {"type":"command", "command":"\"$CLAUDE_PROJECT_DIR\"/.claude/hooks/block-local-memory.sh", "timeout": 8, "unknown": {"a": 1}}
      ]},
      {"matcher": "Bash", "hooks": [
        {"type":"command", "command":"python3 hooks/block-sed.py", "statusMessage":"checking"}
      ]},
      {"matcher": "AskUserQuestion", "hooks": [
        {"type":"command", "command":"tmux display-message ready || true"}
      ]}
    ]
  },
  "other": true
}
`

func isolated(t *testing.T) (dir string, o Options) {
	t.Helper()
	dir = t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, ".codex"))
	o = Options{Harness: "claude", Scope: "project", Dir: dir, DB: filepath.Join(dir, "data", "blotter", "blotter.db"), Binary: "/bin/echo", Version: "test", Out: &bytes.Buffer{}, Record: "full"}
	return
}
func writeSettings(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(vaultSettings), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestInstallRoundTrip(t *testing.T) {
	for _, mode := range []string{"all", "except", "fail-open"} {
		t.Run(mode, func(t *testing.T) {
			dir, o := isolated(t)
			p := writeSettings(t, dir)
			if mode == "except" {
				o.Except = []string{"tmux"}
			}
			if mode == "fail-open" {
				o.FailOpen = []string{"tmux"}
			}
			// Recognition requires the real binary's filename; a harmless executable
			// with that name is sufficient because installation never executes it.
			bin := filepath.Join(dir, "blotter")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			o.Binary = bin
			if err := Apply(o); err != nil {
				t.Fatal(err)
			}
			once, _ := os.ReadFile(p)
			hs, err := discover(once)
			if err != nil {
				t.Fatal(err)
			}
			for i, h := range hs {
				args, wrapped := wrappedArgs(h, o.Harness)
				if mode == "except" && i == 2 {
					if wrapped {
						t.Fatal("wrapped excluded hook")
					}
					continue
				}
				if !wrapped {
					t.Fatalf("not recognized: %s", h.Raw)
				}
				want := []string{"block-local-memory", "block-sed", "AskUserQuestion-3"}[i]
				if argValue(args, "--guard") != want {
					t.Fatal(args)
				}
				if mode == "fail-open" && i == 2 && h.result().Get("onFailure").Exists() {
					t.Fatal("fail-open policy changed")
				}
			}
			if err = Apply(o); err != nil {
				t.Fatal(err)
			}
			twice, _ := os.ReadFile(p)
			if !bytes.Equal(once, twice) {
				t.Fatal("not idempotent")
			}
			o.Uninstall = true
			if err = Apply(o); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(p)
			if string(restored) != vaultSettings {
				t.Fatalf("round trip changed bytes:\n%s", restored)
			}
		})
	}
}
func TestDryRunWritesNothing(t *testing.T) {
	dir, o := isolated(t)
	writeSettings(t, dir)
	o.DryRun = true
	if err := Apply(o); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{o.DB, store.StateDir(), store.DataDir()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("dry run created %s", p)
		}
	}
}
func TestEditsAndSymlinks(t *testing.T) {
	dir, o := isolated(t)
	p := writeSettings(t, dir)
	real := filepath.Join(dir, "dotfiles.json")
	if err := os.Rename(p, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, p); err != nil {
		t.Fatal(err)
	}
	o.Binary = filepath.Join(dir, "blotter")
	if err := os.WriteFile(o.Binary, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Apply(o); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(real)
	edited := bytes.Replace(raw, []byte(`"--guard","block-sed"`), []byte(`"--guard","edited"`), 1)
	if bytes.Equal(raw, edited) {
		t.Fatal("fixture edit failed")
	}
	os.WriteFile(real, edited, 0600)
	o.Uninstall = true
	if err := Apply(o); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(real)
	if !bytes.Contains(after, []byte(`"--guard","edited"`)) {
		t.Fatal("overwrote edited handler")
	}
	info, _ := os.Lstat(p)
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink lost")
	}
}
func TestConcurrentInstall(t *testing.T) {
	dir, o := isolated(t)
	p := writeSettings(t, dir)
	o.Binary = filepath.Join(dir, "blotter")
	os.WriteFile(o.Binary, []byte("#!/bin/sh\n"), 0700)
	o.Out = nil
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { errs <- Apply(o) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(p)
	hs, _ := discover(raw)
	if len(hs) != 3 {
		t.Fatal(string(raw))
	}
	all, err := readManifests(o.DB)
	if err != nil || len(all) != 3 {
		t.Fatalf("%d %v", len(all), err)
	}
}
func TestCodexTrustRoundTrip(t *testing.T) {
	dir, o := isolated(t)
	o.Harness = "codex"
	o.Scope = "user"
	p := filepath.Join(codexHome(), "hooks.json")
	os.MkdirAll(filepath.Dir(p), 0700)
	raw := `{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"/bin/echo ok","timeout":0,"async":true,"statusMessage":"check","additionalContextLimit":0}]}]}}` + "\n"
	os.WriteFile(p, []byte(raw), 0600)
	hs, _ := discover([]byte(raw))
	hash, err := CodexHash(hs[0].Matcher, hs[0].Raw)
	if err != nil {
		t.Fatal(err)
	}
	key := trustKey(p, hs[0])
	original := "# keep me\n[hooks.state." + strconvQuote(key) + "] # header\nenabled = false\ntrusted_hash = " + strconvQuote(hash) + " # approval\n\n[other]\ntext = '''[hooks.state.fake]'''\n"
	config := filepath.Join(codexHome(), "config.toml")
	os.WriteFile(config, []byte(original), 0600)
	o.Binary = filepath.Join(dir, "blotter")
	os.WriteFile(o.Binary, []byte("#!/bin/sh\n"), 0700)
	if err = Apply(o); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(config)
	current, disabled, err := trustState(b, key)
	if err != nil || !disabled || current == hash {
		t.Fatalf("%s %t %v\n%s", current, disabled, err, b)
	}
	o.Uninstall = true
	if err = Apply(o); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(config)
	if string(b) != original {
		t.Fatalf("TOML round trip changed:\n%s", b)
	}
	b, _ = os.ReadFile(p)
	if string(b) != raw {
		t.Fatal("JSON round trip changed")
	}
}
func strconvQuote(s string) string { b := &bytes.Buffer{}; jsonString(b, s); return b.String() }
func TestRecoveryAndBackups(t *testing.T) {
	dir, o := isolated(t)
	a := writeSettings(t, dir)
	c, _ := readChange(a)
	if err := backup(c, o); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, "other", ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(second), 0700)
	os.WriteFile(second, []byte("{}\n"), 0600)
	d, _ := readChange(second)
	if err := backup(d, o); err != nil {
		t.Fatal(err)
	}
	b, err := newestBackup(c.Path)
	if err != nil || string(b) != vaultSettings {
		t.Fatal(err)
	}
	b, err = newestBackup(d.Path)
	if err != nil || string(b) != "{}\n" {
		t.Fatal(err)
	}
	c.After = []byte("new\n")
	os.WriteFile(a, c.After, 0600)
	pendingData := fmtJSON(pending{ID: "interrupted", DB: o.DB, Changes: []change{c}})
	os.MkdirAll(store.StateDir(), 0700)
	os.WriteFile(journalPath(), pendingData, 0600)
	if err = recoverPending(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(a)
	if string(b) != vaultSettings {
		t.Fatal("recovery failed")
	}
	s, err := store.Open(context.Background(), o.DB)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}
func fmtJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func TestBinaryPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "home brew", "Cellar", "blotter", "1.2.3", "bin", "blotter")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, []byte("x"), 0700)
	got, err := BinaryPath(p)
	canonicalDir, _ := filepath.EvalSymlinks(dir)
	want := filepath.Join(canonicalDir, "home brew", "opt", "blotter", "bin", "blotter")
	if err != nil || got != want {
		t.Fatal(got, err)
	}
}
func TestUntrustedDoesNotGrantTrust(t *testing.T) {
	dir, o := isolated(t)
	o.Harness = "codex"
	o.Scope = "user"
	p := filepath.Join(codexHome(), "hooks.json")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, []byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"/bin/echo yes"}]}]}}`), 0600)
	if err := Apply(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatal("untrusted hook created trust file")
	}
}

func TestCompareBeforeWriteAndRollback(t *testing.T) {
	dir, o := isolated(t)
	path := writeSettings(t, dir)
	first, err := readChange(path)
	if err != nil {
		t.Fatal(err)
	}
	first.After = []byte("{\"changed\":true}\n")
	if err = os.WriteFile(path, []byte("user edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = replace(first); err == nil {
		t.Fatal("overwrote concurrent edit")
	}
	if b, _ := os.ReadFile(path); string(b) != "user edit\n" {
		t.Fatal("lost edit")
	}
	if err = os.WriteFile(path, first.Before, 0600); err != nil {
		t.Fatal(err)
	}
	second := change{Path: filepath.Join(dir, "second.json"), Before: []byte("old"), After: []byte("new"), Exists: true, Mode: 0600}
	if err = os.WriteFile(second.Path, []byte("other edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = commitChanges([]change{first, second}, nil, o); err == nil {
		t.Fatal("expected second-file concurrency failure")
	}
	if b, _ := os.ReadFile(path); string(b) != vaultSettings {
		t.Fatal("first write not rolled back")
	}
	if b, _ := os.ReadFile(second.Path); string(b) != "other edit" {
		t.Fatal("rollback overwrote foreign edit")
	}
}
func TestBackupRetentionAndRestore(t *testing.T) {
	dir, o := isolated(t)
	path := writeSettings(t, dir)
	c, err := readChange(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		c.Before = []byte(fmt.Sprintf("{\"n\":%d}\n", i))
		if err = backup(c, o); err != nil {
			t.Fatal(err)
		}
	}
	files, err := backupFiles(backupDir(c.Path))
	if err != nil || len(files) != 10 {
		t.Fatal(len(files), err)
	}
	o.Uninstall = true
	o.Restore = true
	if err = Apply(o); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "{\"n\":11}\n" {
		t.Fatal(string(b))
	}
}
func TestDoctorResolvesProjectThroughSettingsSymlink(t *testing.T) {
	dir, o := isolated(t)
	bin := filepath.Join(dir, "testbin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf '2.1.295 (Claude Code)\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	program := filepath.Join(dir, "hooks", "guard.sh")
	os.MkdirAll(filepath.Dir(program), 0700)
	os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0700)
	path := writeSettings(t, dir)
	raw := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"\"$CLAUDE_PROJECT_DIR\"/hooks/guard.sh"}]}]}}` + "\n"
	os.WriteFile(path, []byte(raw), 0600)
	external := filepath.Join(dir, "dotfiles", "settings.json")
	os.MkdirAll(filepath.Dir(external), 0700)
	os.Rename(path, external)
	os.Symlink(external, path)
	o.Binary = filepath.Join(dir, "blotter")
	os.WriteFile(o.Binary, []byte("#!/bin/sh\n"), 0700)
	if err := Apply(o); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	healthy, err := Doctor(o.DB, dir, &out)
	if err != nil || !healthy {
		t.Fatalf("%v %t\n%s", err, healthy, &out)
	}
}

func TestWrapperFlagsEndAtGuard(t *testing.T) {
	for _, name := range []string{"guard", "--", "--shell-words", "--log"} {
		args := []string{"/path/blotter", "wrap", "--harness", "claude", "--guard", name, "--", "/guard", "--guard", "foreign", "--shell-words", "--log", "all"}
		if got := argValue(args, "--guard"); got != name {
			t.Fatalf("%q: got guard %q", name, got)
		}
		if hasShellWords(args) {
			t.Fatalf("%q: read guard arguments as wrapper flags", name)
		}
		_, command := splitWrappedArgs(args)
		if len(command) != 6 || command[0] != "/guard" {
			t.Fatal(command)
		}
	}
}
