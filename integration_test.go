package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jdevera/blotter/internal/store"
)

var testBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "blotter-build-")
	if err != nil {
		panic(err)
	}
	testBinary = filepath.Join(dir, "blotter")
	cmd := exec.Command("go", "build", "-o", testBinary, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %s %v\n", b, err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
func testEnv(t testing.TB) ([]string, string) {
	t.Helper()
	dir := t.TempDir()
	env := []string{}
	for _, s := range os.Environ() {
		key, _, _ := strings.Cut(s, "=")
		switch key {
		case "HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "CODEX_HOME", "BLOTTER_DB", "CLAUDE_PROJECT_DIR":
			continue
		}
		env = append(env, s)
	}
	db := filepath.Join(dir, "data", "blotter", "blotter.db")
	env = append(env, "HOME="+dir, "XDG_DATA_HOME="+filepath.Join(dir, "data"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "CODEX_HOME="+filepath.Join(dir, ".codex"), "BLOTTER_DB="+db)
	return env, db
}
func invoke(env []string, input string, args ...string) (string, string, int, time.Duration) {
	cmd := exec.Command(testBinary, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(input)
	var out, errout bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errout
	start := time.Now()
	err := cmd.Run()
	code := 0
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else {
			panic(err)
		}
	}
	return out.String(), errout.String(), code, time.Since(start)
}
func wrapArgs(harness string, extra ...string) []string {
	return append([]string{"wrap", "--harness", harness, "--guard", "fake"}, extra...)
}
func allHits(t *testing.T, db string) []store.Hit {
	t.Helper()
	s, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hits, err := s.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}
func TestWrapPassthrough(t *testing.T) {
	cases := []struct {
		name, script, stdout, stderr, outcome string
		rc                                    int
	}{
		{"pass", "printf 'abc\\000xyz'; printf 'err\\n' >&2", "abc\x00xyz", "err\n", "pass", 0},
		{"one", "printf o; printf e >&2; exit 1", "o", "e", "error", 1}, {"two", "printf o; printf e >&2; exit 2", "o", "e", "block", 2}, {"127", "exit 127", "", "", "error", 127},
		{"signal", "kill -TERM $$", "", "", "error", 143},
		{"deny", `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}'`, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}`, "", "block", 0},
		{"malformed", `printf '{broken'`, `{broken`, "", "error", 0},
		{"bad-schema", `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PostToolUse"}}'`, `{"hookSpecificOutput":{"hookEventName":"PostToolUse"}}`, "", "error", 0},
		{"legacy", `printf '%s' '{"decision":"block"}'`, `{"decision":"block"}`, "", "block", 0},
		{"text", `printf ' \nplain text\n'`, " \nplain text\n", "", "pass", 0},
	}
	for _, harness := range []string{"claude", "codex"} {
		for _, tt := range cases {
			t.Run(harness+"/"+tt.name, func(t *testing.T) {
				env, db := testEnv(t)
				out, stderr, rc, _ := invoke(env, `{"cwd":"/project","session_id":"s","tool_name":"Bash","tool_input":{"command":"actual payload"}}`, wrapArgs(harness, "--log", "all", "--sh", tt.script)...)
				if out != tt.stdout || stderr != tt.stderr || rc != tt.rc {
					t.Fatalf("got (%q,%q,%d) want (%q,%q,%d)", out, stderr, rc, tt.stdout, tt.stderr, tt.rc)
				}
				hits := allHits(t, db)
				if len(hits) != 1 || hits[0].Outcome != tt.outcome || hits[0].Payload != "actual payload" || hits[0].RC != rc {
					t.Fatalf("%+v", hits)
				}
			})
		}
	}
}
func TestWrapLaunchFailures(t *testing.T) {
	for _, mode := range []string{"direct", "sh"} {
		t.Run(mode, func(t *testing.T) {
			env, db := testEnv(t)
			args := wrapArgs("codex", "--sh", "/missing/blotter-guard")
			wantRC := 127
			if mode == "direct" {
				args = wrapArgs("codex", "--", "/missing/blotter-guard")
				wantRC = 2
			}
			out, stderr, rc, _ := invoke(env, "{}", args...)
			if out != "" || rc != wantRC || stderr == "" {
				t.Fatal(out, stderr, rc)
			}
			hits := allHits(t, db)
			if len(hits) != 1 || hits[0].Outcome != "error" {
				t.Fatal(hits)
			}
		})
	}
}
func TestLockedDatabaseDoesNotChangeVerdict(t *testing.T) {
	env, db := testEnv(t)
	s, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err := s.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	out, stderr, rc, elapsed := invoke(env, "{}", wrapArgs("claude", "--sh", "printf out; printf err >&2; exit 2")...)
	if out != "out" || stderr != "err" || rc != 2 {
		t.Fatal(out, stderr, rc)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("locked logging took %s", elapsed)
	}
	dir := filepath.Dir(filepath.Dir(filepath.Dir(db)))
	log, err := os.ReadFile(filepath.Join(dir, "state", "blotter", "errors.log"))
	if err != nil || !bytes.Contains(log, []byte("logging")) {
		t.Fatalf("%s %v", log, err)
	}
}
func TestPipeHolderIsBounded(t *testing.T) {
	env, db := testEnv(t)
	out, stderr, rc, elapsed := invoke(env, "{}", wrapArgs("claude", "--log", "all", "--sh", "sleep 3 & printf done")...)
	if out != "done" || stderr != "" || rc != 0 {
		t.Fatal(out, stderr, rc)
	}
	if elapsed > 1200*time.Millisecond {
		t.Fatalf("pipe holder stalled wrapper: %s", elapsed)
	}
	hits := allHits(t, db)
	if len(hits) != 1 || hits[0].DurationMS >= 300 {
		t.Fatalf("guard duration includes pipe drain: %+v", hits)
	}
}
func TestSignalKillsGuardGroup(t *testing.T) {
	env, _ := testEnv(t)
	ready := filepath.Join(t.TempDir(), "ready")
	script := fmt.Sprintf("trap '' TERM; printf '%%s' $$ > '%s'; while :; do sleep 1; done", ready)
	cmd := exec.Command(testBinary, wrapArgs("claude", "--sh", script)...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader("{}")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	until := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(until) {
		raw, err := os.ReadFile(ready)
		if err == nil {
			fmt.Sscanf(string(raw), "%d", &pid)
			if pid > 0 {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("guard not started")
	}
	start := time.Now()
	cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	e, ok := err.(*exec.ExitError)
	if !ok || e.ExitCode() != 143 {
		t.Fatalf("signal status %v stderr %s", err, &stderr)
	}
	elapsed := time.Since(start)
	if elapsed < 1900*time.Millisecond || elapsed > 3500*time.Millisecond {
		t.Fatal(elapsed)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("guard still alive")
	}
	if out.Len() != 0 || stderr.Len() != 0 {
		t.Fatal(&out, &stderr)
	}
}
func TestCanaryConcurrentAndHash(t *testing.T) {
	env, db := testEnv(t)
	out, stderr, rc, _ := invoke(env, "", "canary")
	if rc != 0 {
		t.Fatal(stderr)
	}
	start := strings.Index(out, "BLOTTER-CANARY-")
	token := out[start : start+len("BLOTTER-CANARY-")+8]
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			privacy := "full"
			if i%2 == 0 {
				privacy = "hash"
			}
			out, stderr, rc, _ := invoke(env, `{"tool_input":{"command":"echo `+token+`"}}`, wrapArgs("codex", "--record", privacy, "--sh", "printf should-not-run")...)
			if out != "" || !strings.Contains(stderr, "BLOCKED by blotter canary") || rc != 2 {
				t.Errorf("%q %q %d", out, stderr, rc)
			}
		})
	}
	wg.Wait()
	hits := allHits(t, db)
	if len(hits) != 6 {
		t.Fatalf("%d hits", len(hits))
	}
	out, stderr, rc, _ = invoke(env, "", "canary", "check", token)
	if rc != 0 || strings.Count(out, token) != 6 {
		t.Fatal(out, stderr, rc)
	}
}
func TestOversizedAndHashCapture(t *testing.T) {
	env, db := testEnv(t)
	_, stderr, rc, _ := invoke(env, strings.Repeat("x", (16<<20)+1), wrapArgs("claude", "--sh", "exit 0")...)
	if rc != 2 || !strings.Contains(stderr, "16 MiB") {
		t.Fatal(rc, stderr)
	}
	script := "i=0; while [ $i -lt 7000 ]; do printf 0123456789; i=$((i+1)); done; exit 2"
	out, stderr, rc, _ := invoke(env, `{"tool_input":{"command":"secret"}}`, wrapArgs("claude", "--record", "hash", "--sh", script)...)
	if len(out) != 70000 || stderr != "" || rc != 2 {
		t.Fatal(len(out), stderr, rc)
	}
	hits := allHits(t, db)
	if len(hits) != 1 || !hits[0].Truncated || hits[0].Stdout != "" || hits[0].Event != "" || hits[0].Payload != store.Hash("secret") {
		t.Fatal(hits)
	}
}
func TestPassDoesNotCreateStorage(t *testing.T) {
	env, db := testEnv(t)
	out, stderr, rc, _ := invoke(env, "{}", wrapArgs("claude", "--", "/usr/bin/true")...)
	if out != "" || stderr != "" || rc != 0 {
		t.Fatal(out, stderr, rc)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatal("unlogged pass created database")
	}
}
func TestRecordQueriesAndDoctor(t *testing.T) {
	env, db := testEnv(t)
	for _, args := range [][]string{{"record", "--guard", "g", "--harness", "custom", "--outcome", "error", "--payload", "example", "--project", "/repo/a"}, {"ls", "--project", "/repo/a", "--json"}, {"stats", "--by", "guard,day", "--json"}, {"show", "1"}, {"doctor"}} {
		out, stderr, rc, _ := invoke(env, "", args...)
		if rc != 0 {
			t.Fatalf("%v: %s %s %d", args, out, stderr, rc)
		}
		if slices.Contains(args, "--json") && !json.Valid([]byte(out)) {
			t.Fatal(out)
		}
	}
	if len(allHits(t, db)) != 1 {
		t.Fatal("doctor modified hits")
	}
}
func BenchmarkWrapPass(b *testing.B) {
	env, _ := testEnv(b)
	b.ResetTimer()
	for b.Loop() {
		_, stderr, rc, _ := invoke(env, "{}", wrapArgs("claude", "--", "/usr/bin/true")...)
		if rc != 0 {
			b.Fatal(stderr)
		}
	}
}

func TestStdinEnvironmentAndArguments(t *testing.T) {
	env, db := testEnv(t)
	dir := t.TempDir()
	program := filepath.Join(dir, "guard with spaces.sh")
	script := "#!/bin/sh\ncat\nprintf '\\000%s\\000%s' \"$1\" \"$BLOTTER_TEST_VALUE\"\n"
	if err := os.WriteFile(program, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	env = append(env, "BLOTTER_TEST_PROGRAM="+program, "BLOTTER_TEST_VALUE=inherited", "CLAUDE_PROJECT_DIR=/preferred")
	raw := "  {\"cwd\":\"/event\",\"tool_input\":{\"command\":\"blocked\"}}\n"
	args := wrapArgs("claude", "--log", "all", "--shell-words", "--", `"$BLOTTER_TEST_PROGRAM"`, `'one argument'`)
	out, stderr, rc, _ := invoke(env, raw, args...)
	if out != raw+"\x00one argument\x00inherited" || stderr != "" || rc != 0 {
		t.Fatal(out, stderr, rc)
	}
	hits := allHits(t, db)
	if len(hits) != 1 || hits[0].Event != raw || hits[0].Project != "/preferred" || hits[0].CWD != "/event" {
		t.Fatal(hits)
	}
}
func TestUnknownCanaryRunsGuard(t *testing.T) {
	env, _ := testEnv(t)
	out, stderr, rc, _ := invoke(env, `{"tool_input":{"command":"BLOTTER-CANARY-12345678"}}`, wrapArgs("claude", "--sh", "printf ran")...)
	if out != "ran" || stderr != "" || rc != 0 {
		t.Fatal(out, stderr, rc)
	}
}
func TestHarnessSpecificOutputs(t *testing.T) {
	cases := []struct{ output, claude, codex string }{
		{`{"continue":false}`, "block", "error"},
		{`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask"}}`, "ask", "error"},
		{" \n {\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"deny\"}}\n", "block", "block"},
	}
	for _, tt := range cases {
		for harness, want := range map[string]string{"claude": tt.claude, "codex": tt.codex} {
			t.Run(harness+"/"+want, func(t *testing.T) {
				env, db := testEnv(t)
				out, stderr, rc, _ := invoke(env, "{}", wrapArgs(harness, "--log", "all", "--", "/usr/bin/printf", "%s", tt.output)...)
				if out != tt.output || stderr != "" || rc != 0 {
					t.Fatal(out, stderr, rc)
				}
				hits := allHits(t, db)
				if len(hits) != 1 || hits[0].Outcome != want {
					t.Fatal(hits)
				}
			})
		}
	}
}

func TestSignalDuringPipeDrain(t *testing.T) {
	env, _ := testEnv(t)
	ready := filepath.Join(t.TempDir(), "main-exiting")
	script := fmt.Sprintf("sleep 10 & printf ready; printf done > '%s'", ready)
	cmd := exec.Command(testBinary, wrapArgs("codex", "--sh", script)...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader("{}")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Let the main process exit while its descendant retains stdout/stderr.
	time.Sleep(30 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	e, ok := err.(*exec.ExitError)
	if !ok || e.ExitCode() != 129 || out.String() != "ready" || stderr.Len() != 0 {
		t.Fatalf("status=%v out=%q err=%q", err, out.String(), stderr.String())
	}
}

func TestDirectArgumentsStayLiteral(t *testing.T) {
	env, db := testEnv(t)
	values := []string{"one argument", "$HOME", "${CLAUDE_PROJECT_DIR}", "$(touch should-not-run)", "*", `quotes '" stay`, "--guard", "other", "--log", "all", "--sh", "exit 9"}
	args := wrapArgs("claude", "--log", "all", "--", "/usr/bin/printf", "%s\\000")
	args = append(args, values...)
	out, stderr, rc, _ := invoke(env, "{}", args...)
	want := strings.Join(values, "\x00") + "\x00"
	if out != want || stderr != "" || rc != 0 {
		t.Fatalf("got %q %q %d want %q", out, stderr, rc, want)
	}
	hits := allHits(t, db)
	if len(hits) != 1 || hits[0].Guard != "fake" {
		t.Fatal(hits)
	}
}

func TestInstalledCommandsUsePositionalArguments(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		for _, mode := range []string{"shell-words", "direct"} {
			if harness == "codex" && mode == "direct" {
				continue
			}
			t.Run(harness+"/"+mode, func(t *testing.T) {
				env, _ := testEnv(t)
				dir := t.TempDir()
				settings := filepath.Join(dir, "."+harness, "settings.json")
				if harness == "codex" {
					settings = filepath.Join(dir, ".codex", "hooks.json")
				}
				if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
					t.Fatal(err)
				}
				program := filepath.Join(dir, "guard with spaces'$.sh")
				if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s\\000' \"$@\"\n"), 0700); err != nil {
					t.Fatal(err)
				}
				values := []string{"one argument", `quotes '" stay`, "$HOME", "$(no)", "--guard", "other"}
				original := map[string]any{"type": "command"}
				if mode == "shell-words" {
					original["command"] = `"$BLOTTER_TEST_PROGRAM" 'one argument' 'quotes '\''" stay' '$HOME' '$(no)' --guard other`
					if harness == "codex" {
						original["args"] = []string{"foreign field, ignored by Codex"}
					}
				} else {
					original["command"] = program
					original["args"] = values
				}
				raw, err := json.Marshal(map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{"hooks": []any{original}}}}})
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, '\n')
				if err = os.WriteFile(settings, raw, 0600); err != nil {
					t.Fatal(err)
				}
				// Installation must not resolve this variable or execute the guard.
				installEnv := append(append([]string{}, env...), "BLOTTER_TEST_PROGRAM=/missing/at-install")
				out, stderr, rc, _ := invoke(installEnv, "", "install", harness, "--scope", "project", "--dir", dir)
				if rc != 0 {
					t.Fatalf("install: %s %s %d", out, stderr, rc)
				}
				data, err := os.ReadFile(settings)
				if err != nil {
					t.Fatal(err)
				}
				out, stderr, rc, _ = invoke(installEnv, "", "install", harness, "--scope", "project", "--dir", dir)
				if rc != 0 {
					t.Fatalf("second install: %s %s %d", out, stderr, rc)
				}
				if again, err := os.ReadFile(settings); err != nil || !bytes.Equal(again, data) {
					t.Fatalf("second install changed settings: %s %v", again, err)
				}
				var config struct {
					Hooks map[string][]struct {
						Hooks []struct {
							Command string   `json:"command"`
							Args    []string `json:"args"`
						} `json:"hooks"`
					} `json:"hooks"`
				}
				if err = json.Unmarshal(data, &config); err != nil {
					t.Fatal(err)
				}
				entry := config.Hooks["PreToolUse"][0].Hooks[0]
				var cmd *exec.Cmd
				if harness == "claude" {
					cmd = exec.Command(entry.Command, entry.Args...)
				} else {
					cmd = exec.Command("/bin/sh", "-c", entry.Command)
				}
				cmd.Env = append(append([]string{}, env...), "BLOTTER_TEST_PROGRAM="+program)
				cmd.Dir = dir
				cmd.Stdin = strings.NewReader("{}")
				var output, errors bytes.Buffer
				cmd.Stdout = &output
				cmd.Stderr = &errors
				if err = cmd.Run(); err != nil {
					t.Fatalf("wrapped command: %v\n%s\n%s", err, &output, &errors)
				}
				if want := strings.Join(values, "\x00") + "\x00"; output.String() != want || errors.Len() != 0 {
					t.Fatalf("got %q %q want %q", output.String(), errors.String(), want)
				}
				out, stderr, rc, _ = invoke(installEnv, "", "uninstall", harness, "--scope", "project", "--dir", dir)
				if rc != 0 {
					t.Fatalf("uninstall: %s %s %d", out, stderr, rc)
				}
				restored, err := os.ReadFile(settings)
				if err != nil || !bytes.Equal(restored, raw) {
					t.Fatalf("round trip: %s %v", restored, err)
				}
			})
		}
	}
}
func TestClosedOutputStillLogs(t *testing.T) {
	env, db := testEnv(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	defer w.Close()
	// The guard first checks that it did not inherit an ignored SIGPIPE: with
	// it ignored, yes reports a broken pipe instead of dying silently.
	probe := filepath.Join(t.TempDir(), "yes.err")
	cmd := exec.Command(testBinary, wrapArgs("claude", "--sh", "yes 2>"+probe+" | head -c1 >/dev/null; cat "+probe+"; echo denied >&2; exit 2")...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"x"}}`)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = w
	err = cmd.Run()
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 2 {
		t.Fatalf("want exit 2, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("guard inherited a SIGPIPE disposition: %q", &out)
	}
	if hits := allHits(t, db); len(hits) != 1 || hits[0].Outcome != "block" {
		t.Fatalf("%+v", hits)
	}
}
