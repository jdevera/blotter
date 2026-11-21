package cli

import (
	"bytes"
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
