package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdevera/blotter/internal/guard"
	"github.com/jdevera/blotter/internal/store"
)

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("test writer panic") }
func TestFinalizePanicPreservesStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	var stderr bytes.Buffer
	a := &app{out: panicWriter{}, err: &stderr}
	err := a.passthrough(guard.Result{RC: 7, Stdout: []byte("out")}, store.Hit{Guard: "g"}, nil, "full")
	if err != statusError(7) || stderr.Len() != 0 {
		t.Fatal(err, &stderr)
	}
	b, e := os.ReadFile(filepath.Join(dir, "blotter", "errors.log"))
	if e != nil || !bytes.Contains(b, []byte("recovered panic")) {
		t.Fatal(string(b), e)
	}
}
func TestWrapParseErrorsBlock(t *testing.T) {
	for _, args := range [][]string{
		{"wrap", "--unknown"},
		{"wrap", "--harness", "codex", "--guard", "g", "--"},
		{"wrap", "--harness", "claude", "--guard", "g", "/usr/bin/true"},
		{"wrap", "--harness", "claude", "--guard", "g", "--sh", "exit 0", "--", "/usr/bin/true"},
		{"wrap", "--harness", "claude", "--guard", "g", "--sh", "exit 0", "--shell-words"},
		{"wrap", "--harness", "claude", "--guard", "g", "--argv", "[]"},
		{"--db", "/missing/path", "wrap", "--guard", "g", "--harness", "no"},
	} {
		var out, stderr bytes.Buffer
		code := Run(args, "test", strings.NewReader("{}"), &out, &stderr)
		if code != 2 || out.Len() != 0 || !strings.HasPrefix(stderr.String(), "BLOCKED: blotter could not run guard ") {
			t.Fatal(args, code, &out, &stderr)
		}
	}
}
func TestBlockedMessageNamesWrapperGuard(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("BLOTTER_DB", filepath.Join(dir, "blotter.db"))
	var out, stderr bytes.Buffer
	// An invalid --harness fails before cobra's parsed --guard is available.
	code := Run([]string{"wrap", "--harness", "none", "--guard", "mine", "--", "/bin/guard", "--guard", "theirs"}, "test", strings.NewReader("{}"), &out, &stderr)
	if code != 2 || !strings.HasPrefix(stderr.String(), "BLOCKED: blotter could not run guard mine:") {
		t.Fatal(code, &stderr)
	}
}
func TestShowUnknownID(t *testing.T) {
	t.Setenv("BLOTTER_DB", filepath.Join(t.TempDir(), "blotter.db"))
	var out, stderr bytes.Buffer
	if code := Run([]string{"show", "42"}, "test", strings.NewReader(""), &out, &stderr); code != 1 || stderr.String() != "blotter: no hit with id 42\n" {
		t.Fatal(code, &stderr)
	}
}
func TestListedIDsWorkWithShow(t *testing.T) {
	t.Setenv("BLOTTER_DB", filepath.Join(t.TempDir(), "blotter.db"))
	run := func(args ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := Run(args, "test", strings.NewReader(""), &out, &stderr); code != 0 {
			t.Fatal(args, code, &stderr)
		}
		return out.String()
	}
	for i := range 10 {
		run("record", "--guard", fmt.Sprintf("g%d", i), "--harness", "custom", "--outcome", "block", "--payload", "p")
	}
	lines := strings.Split(strings.TrimSuffix(run("ls"), "\n"), "\n")
	if len(lines) != 10 || !strings.HasPrefix(lines[0], "10  ") || !strings.HasPrefix(lines[9], " 1  ") {
		t.Fatalf("ids missing or misaligned:\n%s", strings.Join(lines, "\n"))
	}
	if shown := run("show", strings.TrimSpace(lines[9][:2])); !strings.Contains(shown, `"guard": "g0"`) {
		t.Fatal(shown)
	}
}
func TestUsageErrorsExplainThemselves(t *testing.T) {
	t.Setenv("BLOTTER_DB", filepath.Join(t.TempDir(), "blotter.db"))
	for _, tt := range []struct {
		args       []string
		want, line string
	}{
		{[]string{"show"}, "blotter: show takes one hit id; blotter ls lists them", "Usage: blotter show <id> [flags]"},
		{[]string{"show", "abc"}, `blotter: "abc" is not a hit id; blotter ls lists them`, "Usage: blotter show <id> [flags]"},
		{[]string{"install", "claudee"}, `blotter: harness must be claude or codex, not "claudee"`, "Usage: blotter install claude|codex [flags]"},
		{[]string{"install", "codex", "--scope", "local"}, `blotter: --scope must be user or project, not "local"`, "Usage: blotter install claude|codex [flags]"},
		{[]string{"ls", "--limit", "abc"}, `blotter: invalid argument "abc" for "--limit" flag: not a number`, "Usage: blotter ls [flags]"},
		{[]string{"ls", "--outcome", "blocked"}, `blotter: --outcome must be block, error, ask, pass or canary, not "blocked"`, "Usage: blotter ls [flags]"},
		{[]string{"stats", "--by", "week"}, `blotter: --by must be guard, day or guard,day, not "week"`, "Usage: blotter stats [flags]"},
		{[]string{"record", "--guard", "g", "--harness", "h"}, "blotter: --outcome is required: block, error, ask, pass or canary", "Usage: blotter record [flags]"},
	} {
		var out, stderr bytes.Buffer
		code := Run(tt.args, "test", strings.NewReader(""), &out, &stderr)
		lines := strings.Split(stderr.String(), "\n")
		if code != 1 || len(lines) < 3 || lines[0] != tt.want || lines[1] != tt.line || !strings.HasPrefix(lines[2], "Run 'blotter ") {
			t.Errorf("%q: exit %d\n%s", tt.args, code, &stderr)
		}
	}
	var out, stderr bytes.Buffer
	if code := Run([]string{"show", "99"}, "test", strings.NewReader(""), &out, &stderr); code != 1 || stderr.String() != "blotter: no hit with id 99\n" {
		t.Errorf("runtime error printed usage: %q", &stderr)
	}
}
func TestCompletions(t *testing.T) {
	db := filepath.Join(t.TempDir(), "blotter.db")
	t.Setenv("BLOTTER_DB", db)
	complete := func(args ...string) []string {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := Run(append([]string{"__complete"}, args...), "test", strings.NewReader(""), &out, &stderr); code != 0 {
			t.Fatal(args, code, &stderr)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		return lines[:len(lines)-1] // drop the directive line
	}
	if got := complete("show", ""); len(got) != 0 {
		t.Fatalf("ids without a database: %q", got)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatal("completion created the database")
	}
	var out, stderr bytes.Buffer
	Run([]string{"record", "--guard", "block-sed", "--harness", "claude", "--outcome", "block"}, "test", strings.NewReader(""), &out, &stderr)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"install", ""}, "claude codex"},
		{[]string{"install", "codex", "--scope", ""}, "user project"},
		{[]string{"ls", "--outcome", "block,e"}, "block,error block,ask block,pass block,canary"},
		{[]string{"stats", "--by", ""}, "guard day guard,day"},
		{[]string{"ls", "--guard", ""}, "block-sed"},
		{[]string{"show", ""}, "1\tblock-sed block "},
	} {
		if got := strings.Join(complete(tt.args...), " "); !strings.HasPrefix(got, tt.want) {
			t.Errorf("%q: got %q want %q", tt.args, got, tt.want)
		}
	}
}
