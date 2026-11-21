package guard

import (
	"reflect"
	"testing"
)

func TestClassification(t *testing.T) {
	tests := []struct {
		name               string
		rc                 int
		out, claude, codex string
	}{
		{"empty", 0, "", "pass", "pass"}, {"one", 1, "", "error", "error"}, {"two", 2, "garbage", "block", "block"}, {"127", 127, "", "error", "error"}, {"signal", 143, "", "error", "error"},
		{"deny", 0, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}`, "block", "block"},
		{"allow", 0, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`, "pass", "pass"},
		{"legacy", 0, `{"decision":"block"}`, "block", "block"},
		{"stop", 0, `{"continue":false}`, "block", "error"}, {"continue", 0, `{"continue":true}`, "pass", "error"},
		{"ask", 0, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask"}}`, "ask", "error"},
		{"bad-json", 0, `{broken`, "error", "error"}, {"bad-decision", 0, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"never"}}`, "error", "error"},
		{"bad-event", 0, `{"hookSpecificOutput":{"hookEventName":"PostToolUse"}}`, "error", "error"},
		{"bad-type", 0, `{"continue":"false"}`, "error", "error"},
		{"text", 0, "text\n", "pass", "pass"}, {"space", 0, " \n {\"decision\":\"block\"}\t", "block", "block"},
		{"suppress", 0, `{"suppressOutput":false}`, "pass", "error"}, {"reason", 0, `{"stopReason":"no"}`, "pass", "error"},
		{"rc-first", 1, `{"decision":"block"}`, "error", "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for h, want := range map[string]string{"claude": tt.claude, "codex": tt.codex} {
				if got := Classify(h, tt.rc, []byte(tt.out)); got != want {
					t.Errorf("%s got %s want %s", h, got, want)
				}
			}
		})
	}
}
func TestWordsAndExpansion(t *testing.T) {
	t.Setenv("CLAUDE_PROJECT_DIR", "/a project")
	t.Setenv("HOME", "/home/test")
	words, err := Words(`"$CLAUDE_PROJECT_DIR"/.claude/hooks/block-sed.py 'one two' "${HOME}" ~/file`)
	if err != nil {
		t.Fatal(err)
	}
	args, err := ExpandWords(words)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/a project/.claude/hooks/block-sed.py", "one two", "/home/test", "/home/test/file"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("%q != %q", args, want)
	}
	for _, s := range []string{"a | b", "a && b", "a; b", "a >file", "X=1 a", "(a)", "a $(touch file)", "a *.go", "cd /tmp", "a &"} {
		w, e := Words(s)
		if e == nil && w != nil {
			t.Errorf("must use shell: %s => %q", s, w)
		}
	}
	if _, err := ExpandWords([]string{"$(touch file)"}); err == nil {
		t.Fatal("accepted command substitution")
	}
}
func TestNames(t *testing.T) {
	for _, tt := range []struct{ command, matcher, want string }{{`"$CLAUDE_PROJECT_DIR"/.claude/hooks/block-sed.py`, "Bash", "block-sed"}, {"python3 hooks/block-sed.py", "Bash", "block-sed"}, {"uv run hooks/guard.py", "Bash", "guard"}, {"env python3 hooks/guard.py", "Bash", "guard"}, {"tmux x || true", "AskUserQuestion", "AskUserQuestion-3"}} {
		w, e := Words(tt.command)
		if e != nil {
			t.Fatal(e)
		}
		if got := Name(w, tt.matcher, 3); got != tt.want {
			t.Errorf("%s: %s != %s", tt.command, got, tt.want)
		}
	}
}
func TestClaudePlaceholders(t *testing.T) {
	raw := []string{"/bin/echo", "${CLAUDE_PROJECT_DIR}/file", "$HOME", "$(no)", "a b"}
	got := ClaudePlaceholders(raw, []string{"CLAUDE_PROJECT_DIR=/a b"})
	want := []string{"/bin/echo", "/a b/file", "$HOME", "$(no)", "a b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if raw[1] != "${CLAUDE_PROJECT_DIR}/file" {
		t.Fatal("mutated stored arguments")
	}
}
