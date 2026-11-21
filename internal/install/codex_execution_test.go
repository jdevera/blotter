package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This exercises actual hook dispatch against a local fake Responses API. It
// uses no credentials, external model requests, or real harness settings.
func TestCodexTrustExecution(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not installed")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local test server unavailable: %v (run outside the network sandbox)", err)
	}
	var calls atomic.Int32
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method != "POST" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"data":[]}`)
			return
		}
		n := calls.Add(1)
		var item map[string]any
		if n == 1 {
			item = map[string]any{"id": "fc1", "type": "function_call", "call_id": "call1", "name": "exec_command", "arguments": `{"cmd":"printf harmless"}`}
		} else {
			item = map[string]any{"id": "msg1", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []any{map[string]any{"type": "response.created", "response": map[string]any{"id": fmt.Sprint("resp", n)}}, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}, map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprint("resp", n), "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}}} {
			b, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	dir, _ := isolated(t)
	home := codexHome()
	os.MkdirAll(home, 0700)
	home, _ = filepath.EvalSymlinks(home)
	marker := filepath.Join(dir, "hook-fired")
	handlerJSON, _ := json.Marshal(map[string]any{"type": "command", "command": "printf hit >> " + "'" + marker + "'"})
	settings := `{"hooks":{"PreToolUse":[{"hooks":[` + string(handlerJSON) + `]}]}}`
	os.WriteFile(filepath.Join(home, "hooks.json"), []byte(settings), 0600)
	hash, err := CodexHash(nil, string(handlerJSON))
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(home, "hooks.json") + ":pre_tool_use:0:0"
	for _, trusted := range []bool{false, true} {
		calls.Store(0)
		os.Remove(marker)
		seed := hash
		if !trusted {
			seed = "sha256:" + strings.Repeat("0", 64)
		}
		config := fmt.Sprintf("model_provider = 'blotter_test'\nmodel = 'blotter-test'\n[model_providers.blotter_test]\nname = 'Blotter test'\nbase_url = 'http://%s/v1'\nwire_api = 'responses'\nrequires_openai_auth = false\n[hooks.state.%s]\ntrusted_hash = %s\n", listener.Addr(), strconvQuote(key), strconvQuote(seed))
		os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmd := exec.CommandContext(ctx, "codex", "exec", "--skip-git-repo-check", "--ephemeral", "-s", "danger-full-access", "Run the shell command supplied by the test server.")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "OPENAI_API_KEY=", "CODEX_API_KEY=", "CODEX_HOME="+home)
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("Codex local-model run (trusted=%t): %v\n%s", trusted, err, output)
		}
		_, err = os.Stat(marker)
		fired := err == nil
		if fired != trusted {
			t.Fatalf("trusted=%t hook fired=%t (requests=%d)\n%s", trusted, fired, calls.Load(), output)
		}
		if calls.Load() < 2 {
			t.Fatalf("vacuous test: only %d model requests", calls.Load())
		}
	}
}
