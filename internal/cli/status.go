package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jdevera/blotter/internal/install"
	"github.com/jdevera/blotter/internal/store"
)

func (a *app) status() *cobra.Command {
	var dir string
	var asJSON bool
	c := &cobra.Command{Use: "status", Short: "Show which hooks apply to a project and whether blotter wraps them", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		files, err := install.Status(store.DBPath(a.db), dir)
		if err != nil {
			return err
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		if asJSON {
			return a.json(struct {
				Dir   string               `json:"dir"`
				Files []install.FileStatus `json:"files"`
			}{abs, files})
		}
		a.printStatus(abs, files, time.Now())
		return nil
	}}
	c.Flags().StringVar(&dir, "dir", ".", "project directory")
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	completeFlag(c, "dir", dirs)
	return c
}

func (a *app) printStatus(dir string, files []install.FileStatus, now time.Time) {
	home, _ := os.UserHomeDir()
	tilde := func(path string) string {
		if home != "" && (path == home || strings.HasPrefix(path, home+string(filepath.Separator))) {
			return "~" + path[len(home):]
		}
		return path
	}
	// Paths under the working directory are relative to it, so they open from
	// the terminal; others start from ~.
	cwd, _ := os.Getwd()
	show := func(path string) string {
		if rel, err := filepath.Rel(cwd, path); cwd != "" && err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
		return tilde(path)
	}
	_, _ = fmt.Fprintf(a.out, "project: %s\n", tilde(dir))
	if dir == home {
		_, _ = fmt.Fprintln(a.out, "  (your home directory, so project and user files coincide)")
	}
	for _, f := range files {
		state := ""
		wrapped := 0
		for _, h := range f.Hooks {
			if h.Wrapped {
				wrapped++
			}
		}
		switch {
		case f.Error != "":
			state = "unreadable: " + f.Error
		case !f.Exists:
			state = "no file"
		case len(f.Hooks) == 0:
			state = "no PreToolUse command hooks"
		default:
			state = fmt.Sprintf("%d of %d hooks wrapped", wrapped, len(f.Hooks))
		}
		_, _ = fmt.Fprintf(a.out, "\n%-6s %-7s  %s  %s\n", f.Harness, f.Scope, show(f.Path), state)
		nameWidth, matcherWidth := 0, 0
		for _, h := range f.Hooks {
			nameWidth = max(nameWidth, len(hookName(h)))
			matcherWidth = max(matcherWidth, len(matcherLabel(h.Matcher)))
		}
		for _, h := range f.Hooks {
			detail := clean(h.Command)
			if h.Wrapped {
				detail = "no hits"
				if h.LastHit != nil {
					detail = fmt.Sprintf("last hit %s (%s)", ago(h.LastHit.TS, now), h.LastHit.Outcome)
				}
				if h.FailOpen {
					detail += ", fail-open"
				}
			}
			_, _ = fmt.Fprintf(a.out, "  %-*s  %-*s  %s\n", nameWidth, hookName(h), matcherWidth, matcherLabel(h.Matcher), detail)
			if h.Line > 0 {
				_, _ = fmt.Fprintf(a.out, "    at %s:%d\n", show(f.Path), h.Line)
			}
			if h.Wrapped {
				_, _ = fmt.Fprintf(a.out, "    runs %s\n", clean(h.Command))
			}
			if h.Problem != "" {
				_, _ = fmt.Fprintf(a.out, "    warning: %s\n", h.Problem)
			}
			if h.WrapCommand != "" {
				_, _ = fmt.Fprintf(a.out, "    wrap it: %s\n", h.WrapCommand)
				if h.WrapAlsoMatches > 0 {
					_, _ = fmt.Fprintf(a.out, "    (this --only also matches %d other unwrapped %s; add --dry-run to check)\n", h.WrapAlsoMatches, plural(h.WrapAlsoMatches, "hook"))
				}
			}
		}
	}
}

func hookName(h install.HookStatus) string {
	if h.Wrapped {
		return h.Guard
	}
	return "unwrapped"
}

func matcherLabel(m string) string {
	if m == "" {
		return "any tool"
	}
	return m
}

func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// ago describes a stored UTC timestamp relative to now.
func ago(ts string, now time.Time) string {
	t, err := time.Parse("2006-01-02T15:04:05.000Z", ts)
	if err != nil {
		return ts
	}
	switch d := now.Sub(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
