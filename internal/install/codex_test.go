package install

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexRustFixtures(t *testing.T) {
	b, err := os.ReadFile("testdata/codex-hashes.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Cases []struct {
			Name    string          `json:"name"`
			Matcher *string         `json:"matcher"`
			Handler json.RawMessage `json:"handler"`
			Hash    string          `json:"hash"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(b, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures.Cases {
		t.Run(f.Name, func(t *testing.T) {
			got, err := CodexHash(f.Matcher, string(f.Handler))
			if err != nil || got != f.Hash {
				t.Fatalf("got %s %v want %s", got, err, f.Hash)
			}
		})
	}
}
func TestCodexEngineTrust(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not installed")
	}
	dir, o := isolated(t)
	_ = o
	b, err := exec.Command("codex", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Log(strings.TrimSpace(string(b)))
	home := codexHome()
	os.MkdirAll(home, 0700)
	raw := `{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"/bin/echo yes","timeout":0,"async":true,"statusMessage":"checking","additionalContextLimit":0}]}]}}`
	os.WriteFile(filepath.Join(home, "hooks.json"), []byte(raw), 0600)
	hs, err := discover([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := CodexHash(hs[0].Matcher, hs[0].Raw)
	if err != nil {
		t.Fatal(err)
	}
	key := trustKey(filepath.Join(home, "hooks.json"), hs[0])
	config := filepath.Join(home, "config.toml")
	for _, trusted := range []bool{true, false} {
		seed := hash
		if !trusted {
			seed = "sha256:" + strings.Repeat("0", 64)
		}
		text := "[hooks.state." + strconvQuote(key) + "]\ntrusted_hash = " + strconvQuote(seed) + "\n"
		if err = os.WriteFile(config, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		list, _, err := CodexInspect(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || len(list[0].Hooks) != 1 {
			t.Fatalf("%+v", list)
		}
		hook := list[0].Hooks[0]
		want := "trusted"
		if !trusted {
			want = "modified"
		}
		if hook.Hash != hash || hook.Trust != want || hook.Key != key {
			t.Fatalf("%+v expected %s %s", hook, hash, want)
		}
	}
}
