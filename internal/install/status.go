package install

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/jdevera/blotter/internal/guard"
)

// FileStatus describes one hook file that applies to a directory.
type FileStatus struct {
	Harness string       `json:"harness"`
	Scope   string       `json:"scope"`
	Path    string       `json:"path"`
	Exists  bool         `json:"exists"`
	Error   string       `json:"error,omitempty"`
	Hooks   []HookStatus `json:"hooks"`
}

// HookStatus describes one PreToolUse command hook and where it is defined.
type HookStatus struct {
	Line     int      `json:"line"`
	Matcher  string   `json:"matcher"`
	Command  string   `json:"command"`
	Wrapped  bool     `json:"wrapped"`
	Guard    string   `json:"guard,omitempty"`
	FailOpen bool     `json:"fail_open,omitempty"`
	Problem  string   `json:"problem,omitempty"`
	LastHit  *LastHit `json:"last_hit,omitempty"`
	// WrapCommand installs blotter around an unwrapped hook.
	WrapCommand string `json:"wrap_command,omitempty"`
	// WrapAlsoMatches counts other unwrapped hooks WrapCommand would select.
	WrapAlsoMatches int `json:"wrap_also_matches,omitempty"`
}

type LastHit struct {
	TS      string `json:"ts"`
	Outcome string `json:"outcome"`
}

// Status reports every hook file that applies when an agent works in dir:
// project and local files for dir, and the user's own files. It reads the
// database only if it exists and never changes anything.
func Status(dbPath, dir string) ([]FileStatus, error) {
	all, err := readManifests(dbPath)
	if err != nil {
		return nil, err
	}
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	if db != nil {
		defer func() { _ = db.Close() }()
	}
	out := []FileStatus{}
	seen := map[string]bool{}
	for _, t := range []struct{ harness, scope string }{
		{"claude", "project"}, {"claude", "local"}, {"claude", "user"}, {"codex", "project"}, {"codex", "user"},
	} {
		path, _, err := targets(Options{Harness: t.harness, Scope: t.scope, Dir: dir})
		if err != nil {
			return nil, err
		}
		fs := FileStatus{Harness: t.harness, Scope: t.scope, Path: path, Hooks: []HookStatus{}}
		c, err := readChange(path)
		if err != nil {
			fs.Error = err.Error()
			out = append(out, fs)
			continue
		}
		if seen[c.Path] {
			continue // the same file through another scope, e.g. a project in $HOME
		}
		seen[c.Path] = true
		fs.Exists = c.Exists
		if !c.Exists {
			out = append(out, fs)
			continue
		}
		handlers, err := discover(c.Before)
		if err != nil {
			fs.Error = err.Error()
			out = append(out, fs)
			continue
		}
		relink(all, c.Path, path, handlers)
		for _, h := range handlers {
			hs := HookStatus{Line: lineOf(c.Before, h.Path), Matcher: h.matcher(), Command: displayCommand(h.Raw)}
			args, wrapped := wrappedArgs(h, t.harness)
			if !wrapped {
				hs.WrapCommand = wrapCommand(t.harness, t.scope, dir, h.Command)
				for _, other := range handlers {
					if _, w := wrappedArgs(other, t.harness); !w && other.Path != h.Path && strings.Contains(other.Command, h.Command) {
						hs.WrapAlsoMatches++
					}
				}
				fs.Hooks = append(fs.Hooks, hs)
				continue
			}
			hs.Wrapped = true
			hs.Guard = argValue(args, "--guard")
			hs.FailOpen = t.harness == "claude" && h.result().Get("onFailure").String() != "block"
			switch idx := manifestIndex(all, c.Path, h); {
			case idx < 0:
				hs.Problem = "no install record; uninstall cannot restore the original hook"
			case all[idx].Wrapped != h.Raw:
				hs.Problem = "edited since install; uninstall leaves it unchanged"
			default:
				hs.Command = displayCommand(all[idx].Original)
			}
			if db != nil {
				var last LastHit
				err := db.QueryRow("SELECT ts, outcome FROM hits WHERE guard=? AND harness=? ORDER BY ts DESC, id DESC LIMIT 1", hs.Guard, t.harness).Scan(&last.TS, &last.Outcome)
				if err == nil {
					hs.LastHit = &last
				} else if !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
			}
			fs.Hooks = append(fs.Hooks, hs)
		}
		out = append(out, fs)
	}
	return out, nil
}

// lineOf returns the 1-based line where the value at path starts.
func lineOf(raw []byte, path string) int {
	r := gjson.GetBytes(raw, path)
	if !r.Exists() || r.Index <= 0 {
		return 0
	}
	return bytes.Count(raw[:r.Index], []byte("\n")) + 1
}

// displayCommand renders a handler's command, including Claude exec-form args.
func displayCommand(raw string) string {
	r := gjson.Parse(raw)
	words := []string{r.Get("command").String()}
	if args := r.Get("args"); args.IsArray() {
		for _, a := range args.Array() {
			words = append(words, shellWord(a.String()))
		}
	}
	return strings.Join(words, " ")
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

func shellWord(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return guard.Quote(s)
}

func wrapCommand(harness, scope, dir, command string) string {
	words := []string{"blotter", "install", harness}
	if scope != "user" {
		words = append(words, "--scope", scope)
		if filepath.Clean(dir) != "." {
			words = append(words, "--dir", shellWord(dir))
		}
	}
	return strings.Join(append(words, "--only", shellWord(command)), " ")
}
