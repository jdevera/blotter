package install

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/jdevera/blotter/internal/guard"
	"github.com/jdevera/blotter/internal/store"
)

func Doctor(dbPath, dir string, out io.Writer) (bool, error) {
	report := &reportWriter{Writer: out}
	out = report
	ok := true
	check := func(pass bool, format string, args ...any) {
		label := "ok"
		if !pass {
			label = "FAIL"
			ok = false
		}
		_, _ = fmt.Fprintf(out, "%s %s\n", label, fmt.Sprintf(format, args...))
	}
	s, err := store.Open(context.Background(), dbPath)
	if err != nil {
		check(false, "database: %v", err)
	} else {
		defer func() { _ = s.Close() }()
		tx, e := s.DB.Begin()
		if e == nil {
			_, e = tx.Exec("CREATE TABLE IF NOT EXISTS doctor_probe(id INTEGER)")
			_ = tx.Rollback()
		}
		if e != nil {
			check(false, "database writability: %v", e)
		} else {
			check(true, "database: schema %d, writable", store.SchemaVersion)
		}
	}
	if _, e := os.Stat(journalPath()); e == nil {
		check(false, "interrupted installation pending; rerun install/uninstall to recover it")
	} else if !errors.Is(e, os.ErrNotExist) {
		check(false, "installation journal: %v", e)
	}
	log, err := os.Open(filepath.Join(store.StateDir(), "errors.log"))
	recent := 0
	if err == nil {
		defer func() { _ = log.Close() }()
		scan := bufio.NewScanner(log)
		scan.Buffer(make([]byte, 4096), 1<<20)
		cutoff := store.Timestamp(time.Now().AddDate(0, 0, -7))
		for scan.Scan() {
			line := scan.Text()
			ts, _, found := strings.Cut(line, " ")
			if !found {
				check(false, "malformed diagnostic: %s", line)
				recent++
				continue
			}
			if ts >= cutoff {
				check(false, "diagnostic: %s", line)
				recent++
			}
		}
		if e := scan.Err(); e != nil {
			check(false, "diagnostics: %v", e)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		check(false, "diagnostics: %v", err)
	}
	if recent == 0 {
		check(true, "no blotter errors in the last 7 days")
	}
	all := []Manifest{}
	if s != nil {
		all, err = manifests(s.DB)
		if err != nil {
			check(false, "manifest: %v", err)
		}
	}
	paths := map[string]Options{}
	for _, h := range []string{"claude", "codex"} {
		for _, scope := range []string{"user", "project", "local"} {
			if h == "codex" && scope == "local" {
				continue
			}
			o := Options{Harness: h, Scope: scope, Dir: dir}
			p, _, e := targets(o)
			if e != nil {
				return false, e
			}
			if _, e = os.Stat(p); e == nil {
				paths[p] = o
			}
		}
	}
	for _, m := range all {
		if _, exists := paths[m.SourceTarget]; !exists {
			paths[m.SourceTarget] = Options{Harness: m.Harness, Scope: m.Scope, Dir: dir}
		}
	}
	seen := map[int64]bool{}
	claudeChecked := false
	codexNeeded := false
	for p, o := range paths {
		c, e := readChange(p)
		if e != nil {
			check(false, "%s: %v", p, e)
			continue
		}
		if !c.Exists {
			check(false, "missing hook file %s", p)
			continue
		}
		handlers, e := discover(c.Before)
		if e != nil {
			check(false, "%s: %v", p, e)
			continue
		}
		for _, h := range handlers {
			args, wrapped := wrappedArgs(h, o.Harness)
			if !wrapped {
				check(true, "%s handler %d is unwrapped: %s", p, h.Position, h.Command)
				continue
			}
			idx := manifestIndex(all, c.Path, h)
			if idx < 0 {
				check(false, "%s handler %d is wrapped without a manifest", p, h.Position)
			} else {
				seen[all[idx].ID] = true
				check(all[idx].Wrapped == h.Raw, "%s manifest matches", all[idx].Guard)
			}
			check(executable(args[0]) == nil, "blotter executable %s", args[0])
			name := argValue(args, "--guard")
			project, _ := filepath.Abs(dir)
			if o.Scope != "user" {
				project = filepath.Dir(filepath.Dir(p))
			}
			environ := []string{}
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "CLAUDE_PROJECT_DIR=") {
					environ = append(environ, entry)
				}
			}
			environ = append(environ, "CLAUDE_PROJECT_DIR="+project)
			_, words := splitWrappedArgs(args)
			var e error
			if hasShellWords(args) {
				words, e = guard.ExpandWordsIn(words, environ)
			} else if o.Harness == "claude" {
				words = guard.ClaudePlaceholders(words, environ)
			}
			if e != nil {
				check(false, "%s guard expansion: %v", name, e)
			} else if len(words) > 0 {
				if !filepath.IsAbs(words[0]) && strings.Contains(words[0], string(filepath.Separator)) {
					words[0] = filepath.Join(project, words[0])
				}
				check(executable(words[0]) == nil, "%s program %s executable in current environment", name, words[0])
			}
			if o.Harness == "claude" {
				if !claudeChecked {
					if v, e := claudeVersion(); e != nil {
						check(false, "Claude onFailure support: %v", e)
					} else if v == "" {
						check(true, "Claude version not checked as root")
					} else {
						check(true, "Claude Code %s supports onFailure", v)
					}
					claudeChecked = true
				}
				exempt := idx >= 0 && all[idx].FailOpen
				check(h.result().Get("onFailure").String() == "block" || exempt, "%s onFailure policy (explicit fail-open=%t)", name, exempt)
			} else {
				codexNeeded = true
			}
			if s != nil {
				timeout := int64(600)
				if v := h.result().Get("timeout"); v.Exists() {
					timeout = max(1, v.Int())
				}
				var duration sql.NullInt64
				e := s.DB.QueryRow("SELECT max(duration_ms) FROM hits WHERE guard=? AND harness=?", name, o.Harness).Scan(&duration)
				if e != nil {
					check(false, "%s duration lookup: %v", name, e)
				} else if duration.Valid {
					check(duration.Int64 <= timeout*500, "%s longest logged run %d ms; half-timeout %d ms", name, duration.Int64, timeout*500)
				}
			}
		}
	}
	for _, m := range all {
		if !seen[m.ID] {
			check(false, "manifest %s has no matching wrapped entry in %s", m.Guard, m.Target)
		}
	}
	if codexNeeded {
		abs, e := filepath.Abs(dir)
		if e != nil {
			return false, e
		}
		directories := map[string]bool{abs: true}
		for _, m := range all {
			if m.Harness == "codex" && m.Scope != "user" {
				directories[filepath.Dir(filepath.Dir(m.SourceTarget))] = true
			}
		}
		found := map[string]CodexHook{}
		for project := range directories {
			list, config, e := CodexInspect(project)
			if e != nil {
				check(false, "effective Codex configuration for %s unavailable: %v", project, e)
				continue
			}
			b, _ := json.Marshal(config)
			feature := gjson.GetBytes(b, "config.features.codex_hooks")
			if !feature.Exists() {
				feature = gjson.GetBytes(b, "config.features.hooks")
			}
			check(!feature.Exists() || feature.Bool(), "Codex hooks feature enabled for %s", project)
			for _, entry := range list {
				for _, e := range entry.Errors {
					check(false, "Codex hooks: %s", e.Message)
				}
				for _, hook := range entry.Hooks {
					found[hook.Key] = hook
				}
			}
		}
		for _, m := range all {
			if m.Harness != "codex" {
				continue
			}
			hook, present := found[m.TrustKey]
			check(present, "%s present in effective Codex hooks (including managed-policy restrictions)", m.Guard)
			if !present {
				continue
			}
			if !hook.Enabled {
				check(true, "%s deliberately disabled in Codex", m.Guard)
				continue
			}
			check(hook.Trust == "trusted" || hook.Trust == "managed", "%s Codex trust %s", m.Guard, hook.Trust)
		}
	}

	if s != nil {
		c, e := s.LatestCanary(context.Background())
		switch {
		case errors.Is(e, sql.ErrNoRows):
			check(true, "no canary created yet")
		case e != nil:
			check(false, "latest canary: %v", e)
		default:
			hits, e := s.CanaryHits(context.Background(), c.Token)
			check(e == nil && len(hits) > 0, "latest canary %s caught %d times", c.Token, len(hits))
		}
	}
	if ok {
		_, _ = fmt.Fprintln(out, "verdict: healthy")
	} else {
		_, _ = fmt.Fprintln(out, "verdict: failures found")
	}
	return ok, report.err
}
func executable(program string) error {
	path, err := exec.LookPath(program)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("not executable")
	}
	return nil
}
