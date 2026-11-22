package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestStorageAndQueries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "private", "hits.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Dir(path), 0700}, {path, 0600}} {
		info, err := os.Stat(p.path)
		if err != nil || info.Mode().Perm() != p.mode {
			t.Fatalf("permissions: %s %v %v", p.path, info, err)
		}
	}
	for _, project := range []string{"/repo/a", "/repo/a/sub", "/repo/abc", "/repo/a_", "/repo/a%"} {
		h := Hit{TS: Timestamp(time.Now()), Guard: "g", Harness: "claude", Outcome: "block", Project: project}
		if err = s.Insert(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	for p, want := range map[string]int{"/repo/a": 2, "/repo/a_": 1, "/repo/a%": 1, "/": 5} {
		rows, err := s.List(ctx, Filter{Project: p})
		if err != nil || len(rows) != want {
			t.Errorf("%s: got %d %v want %d", p, len(rows), err, want)
		}
	}
	rows, err := s.Stats(ctx, Filter{}, "guard")
	if err != nil || len(rows) != 1 || rows[0].Block != 5 {
		t.Fatalf("stats: %+v %v", rows, err)
	}
	var v int
	if err = s.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != SchemaVersion {
		t.Fatal(v, err)
	}
}
func TestPayloadPrivacy(t *testing.T) {
	t.Setenv("CLAUDE_PROJECT_DIR", "/preferred")
	raw := []byte(" {\"cwd\":\"/cwd\",\"session_id\":\"s\",\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"\",\"file_path\":\"file\"}} \n")
	h := FromEvent(raw)
	if h.Payload != "" || h.Event != string(raw) || h.Project != "/preferred" || h.CWD != "/cwd" {
		t.Fatalf("%+v", h)
	}
	h.Stdout = strings.Repeat("x", CaptureCap+1)
	h.Stderr = "secret"
	h.Prepare("hash")
	if h.Payload != Hash("") || h.Event != "" || h.Stdout != "" || h.Stderr != "" || !h.Truncated {
		t.Fatal(h)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	h = FromEvent([]byte(`{"cwd":"/cwd","tool_input":{"x": 2}}`))
	if h.Payload != `{"x":2}` || h.Project != "/cwd" {
		t.Fatal(h)
	}
}
func TestSince(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for input, want := range map[string]string{"7d": "2026-11-14T12:00:00.000Z", "2w": "2026-11-07T12:00:00.000Z", "1h": "2026-11-21T11:00:00.000Z", "2026-11-01": "2026-11-01T00:00:00.000Z"} {
		got, err := Since(input, now)
		if err != nil || got != want {
			t.Errorf("%s: %s %v", input, got, err)
		}
	}
	for _, input := range []string{"-1d", "0h", "no", "9999999999999999999w"} {
		if _, err := Since(input, now); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}
func TestDiagnosticsLog(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	Diagnostic(KindBackground, "my guard\nname", fmt.Errorf("guard x pipe capture:\nWaitDelay"))
	Diagnostic(KindCanary, "", fmt.Errorf("canary lookup: locked"))
	b, err := os.ReadFile(DiagnosticLogs()[1])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%q", b)
	}
	want := []DiagnosticEntry{{"", KindBackground, "my_guard_name", "guard x pipe capture: WaitDelay"}, {"", KindCanary, "", "canary lookup: locked"}}
	for i, line := range lines {
		e, ok := ParseDiagnostic(line)
		e.TS = ""
		if !ok || e != want[i] {
			t.Errorf("%q => %+v", line, e)
		}
	}
	if e, ok := ParseDiagnostic("2026-11-20T00:00:00.000Z guard demo logging: locked"); !ok || e.Kind != KindOther || e.Message != "guard demo logging: locked" {
		t.Errorf("untagged line: %+v", e)
	}
	if err = os.WriteFile(DiagnosticLogs()[1], make([]byte, DiagnosticLogLimit), 0600); err != nil {
		t.Fatal(err)
	}
	Diagnostic(KindPanic, "", fmt.Errorf("boom"))
	if info, err := os.Stat(DiagnosticLogs()[0]); err != nil || info.Size() != DiagnosticLogLimit {
		t.Fatal("log not rotated", err)
	}
	if b, _ = os.ReadFile(DiagnosticLogs()[1]); !strings.Contains(string(b), " panic - boom\n") {
		t.Fatalf("%q", b)
	}
}
func TestTruncationKeepsUTF8(t *testing.T) {
	h := Hit{Stdout: strings.Repeat("x", CaptureCap-1) + "é and more", Stderr: strings.Repeat("\xff", CaptureCap+1)}
	h.Prepare("full")
	if len(h.Stdout) != CaptureCap-1 || !utf8.ValidString(h.Stdout) || !h.Truncated {
		t.Fatalf("stdout cut to %d bytes, valid=%t", len(h.Stdout), utf8.ValidString(h.Stdout))
	}
	if len(h.Stderr) != CaptureCap {
		t.Fatalf("binary stderr cut to %d bytes", len(h.Stderr))
	}
}
func TestHitEventJSON(t *testing.T) {
	for _, tt := range []struct{ event, want string }{
		{" {\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"a && b > c\"}}\n", `{"tool_name":"Bash","tool_input":{"command":"a && b > c"}}`},
		{"", `null`},
		{"{broken", `"{broken"`},
		{"[1,2]", `"[1,2]"`},
		{"42", `"42"`},
		{"plain <text>", `"plain <text>"`},
	} {
		b, err := json.Marshal(Hit{ID: 7, Event: tt.event})
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			ID    int64           `json:"id"`
			Event json.RawMessage `json:"event"`
		}
		if err = json.Unmarshal(b, &got); err != nil || got.ID != 7 {
			t.Fatalf("%s: %v", b, err)
		}
		var compact bytes.Buffer
		if err = json.Compact(&compact, got.Event); err != nil {
			t.Fatal(err)
		}
		// Go's json.Marshal escapes HTML in strings; compare decoded values.
		var gotV, wantV any
		json.Unmarshal(compact.Bytes(), &gotV)
		json.Unmarshal([]byte(tt.want), &wantV)
		if !reflect.DeepEqual(gotV, wantV) || bytes.Count(b, []byte(`"event"`)) != 1 {
			t.Errorf("%q => %s", tt.event, b)
		}
	}
}
