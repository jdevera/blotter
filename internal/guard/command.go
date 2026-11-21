package guard

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// Words returns unexpanded shell words only when executing them directly preserves
// the shell's behavior. Everything else remains a shell command.
func Words(command string) ([]string, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, err
	}
	if len(f.Stmts) != 1 {
		return nil, nil
	}
	s := f.Stmts[0]
	if s.Background || s.Negated || s.Coprocess || s.Semicolon.IsValid() || len(s.Redirs) > 0 {
		return nil, nil
	}
	c, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(c.Assigns) > 0 || len(c.Args) == 0 {
		return nil, nil
	}
	safe := true
	syntax.Walk(c, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp, *syntax.ExtGlob, *syntax.BraceExp:
			safe = false
			return false
		case *syntax.ParamExp:
			if x.Excl || x.Length || x.Width || x.Index != nil || x.Slice != nil || x.Repl != nil || x.Exp != nil {
				safe = false
				return false
			}
		case *syntax.Lit:
			if strings.ContainsAny(x.Value, "*?[") {
				safe = false
				return false
			}
		}
		return true
	})
	if !safe {
		return nil, nil
	}
	out := make([]string, len(c.Args))
	for i, w := range c.Args {
		var b bytes.Buffer
		if err := syntax.NewPrinter().Print(&b, w); err != nil {
			return nil, err
		}
		out[i] = b.String()
	}
	first := c.Args[0].Lit()
	switch first {
	case ".", ":", "[", "alias", "bg", "break", "cd", "command", "continue", "echo", "eval", "exec", "exit", "export", "false", "fc", "fg", "getopts", "hash", "jobs", "kill", "printf", "pwd", "read", "readonly", "return", "set", "shift", "test", "times", "trap", "true", "type", "ulimit", "umask", "unalias", "unset", "wait":
		return nil, nil
	}
	return out, nil
}

func ExpandWords(words []string) ([]string, error) {
	return ExpandWordsIn(words, os.Environ())
}
func ExpandWordsIn(raw []string, environ []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("a guard program is required after --")
	}
	words := make([]*syntax.Word, 0, len(raw))
	for _, w := range raw {
		f, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(w), "")
		if err != nil {
			return nil, err
		}
		if len(f.Stmts) != 1 || len(f.Stmts[0].Redirs) > 0 {
			return nil, fmt.Errorf("invalid shell word %q", w)
		}
		c, ok := f.Stmts[0].Cmd.(*syntax.CallExpr)
		if !ok || len(c.Assigns) > 0 || len(c.Args) != 1 {
			return nil, fmt.Errorf("invalid shell word %q", w)
		}
		checked, err := Words("blotter-word " + w)
		if err != nil || checked == nil {
			return nil, fmt.Errorf("unsupported expansion in %q", w)
		}
		words = append(words, c.Args[0])
	}
	args, err := expand.Fields(&expand.Config{Env: expand.ListEnviron(environ...), ReadDir2: os.ReadDir}, words...)
	if err != nil {
		return nil, err
	}
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("empty guard program")
	}
	return args, nil
}

// ClaudePlaceholders lets doctor inspect exec-form arguments as Claude would.
// Runtime direct arguments have already been processed by the caller.
func ClaudePlaceholders(raw []string, environ []string) []string {
	env := map[string]string{}
	for _, entry := range environ {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			env[k] = v
		}
	}
	args := append([]string(nil), raw...)
	for i, a := range args {
		for _, k := range []string{"CLAUDE_PROJECT_DIR", "CLAUDE_PLUGIN_ROOT", "CLAUDE_PLUGIN_DATA"} {
			if v, ok := env[k]; ok {
				a = strings.ReplaceAll(a, "${"+k+"}", v)
			}
		}
		args[i] = a
	}
	return args
}
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func ShellJoin(args []string) string {
	out := make([]string, len(args))
	for i, s := range args {
		out[i] = Quote(s)
	}
	return strings.Join(out, " ")
}

func Name(words []string, matcher string, position int) string {
	for i := 0; i < len(words); i++ {
		w := strings.Trim(words[i], "\"'")
		base := filepath.Base(w)
		if strings.HasPrefix(w, "-") || strings.Contains(w, "=") {
			continue
		}
		switch base {
		case "sh", "bash", "zsh", "python", "python3", "node", "ruby", "perl", "env":
			continue
		case "uv":
			if i+1 < len(words) && words[i+1] == "run" {
				i++
			}
			continue
		}
		if strings.Contains(w, "/") || filepath.Ext(base) != "" {
			return strings.TrimSuffix(base, filepath.Ext(base))
		}
		break
	}
	if matcher == "" {
		matcher = "any"
	}
	return fmt.Sprintf("%s-%d", matcher, position)
}
