package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
