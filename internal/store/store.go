package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite" // Register the pure-Go SQLite database/sql driver.
)

const SchemaVersion = 1
const CaptureCap = 64 << 10

func Timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func DataDir() string  { return xdg("XDG_DATA_HOME", ".local/share") }
func StateDir() string { return xdg("XDG_STATE_HOME", ".local/state") }
func xdg(key, fallback string) string {
	base := os.Getenv(key)
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, fallback)
	}
	return filepath.Join(base, "blotter")
}
func DBPath(override string) string {
	if override != "" {
		return override
	}
	if p := os.Getenv("BLOTTER_DB"); p != "" {
		return p
	}
	return filepath.Join(DataDir(), "blotter.db")
}

func Diagnostic(err error) {
	if err == nil {
		return
	}
	if os.MkdirAll(StateDir(), 0700) != nil {
		return
	}
	f, e := os.OpenFile(filepath.Join(StateDir(), "errors.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return
	}
	defer func() { _ = f.Close() }()
	msg := strings.NewReplacer("\n", " ", "\r", " ").Replace(err.Error())
	_, _ = fmt.Fprintf(f, "%s %s\n", Timestamp(time.Now()), msg)
}

type Store struct{ DB *sql.DB }

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: "file", Path: path}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		u.Path = abs
	}
	q := url.Values{}
	for _, p := range []string{"busy_timeout(250)", "journal_mode(WAL)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	if err = s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > SchemaVersion {
		return fmt.Errorf("database schema %d is newer than supported schema %d", v, SchemaVersion)
	}
	if v == SchemaVersion {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS hits (
 id INTEGER PRIMARY KEY, ts TEXT NOT NULL, guard TEXT NOT NULL, harness TEXT NOT NULL,
 outcome TEXT NOT NULL, rc INTEGER NOT NULL, stdout TEXT NOT NULL, stderr TEXT NOT NULL,
 truncated INTEGER NOT NULL, duration_ms INTEGER NOT NULL, project TEXT NOT NULL,
 cwd TEXT NOT NULL, session TEXT NOT NULL, tool TEXT NOT NULL, payload TEXT NOT NULL,
 event TEXT NOT NULL, blotter_version TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS hits_ts ON hits(ts);
CREATE INDEX IF NOT EXISTS hits_guard_ts ON hits(guard,ts);
CREATE INDEX IF NOT EXISTS hits_project_ts ON hits(project,ts);
CREATE TABLE IF NOT EXISTS canaries (token TEXT PRIMARY KEY, created TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS installs (
 id INTEGER PRIMARY KEY, harness TEXT NOT NULL, scope TEXT NOT NULL, target TEXT NOT NULL,
 event TEXT NOT NULL, group_index INTEGER NOT NULL, handler_index INTEGER NOT NULL,
 guard TEXT NOT NULL, original TEXT NOT NULL, wrapped TEXT NOT NULL, trust_key TEXT NOT NULL,
 prior_trust TEXT, installed_at TEXT NOT NULL, blotter_version TEXT NOT NULL,
 fail_open INTEGER NOT NULL DEFAULT 0, source_target TEXT NOT NULL,
 UNIQUE(target,event,group_index,handler_index)
);
PRAGMA user_version=1;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type Hit struct {
	ID         int64  `json:"id"`
	TS         string `json:"ts"`
	Guard      string `json:"guard"`
	Harness    string `json:"harness"`
	Outcome    string `json:"outcome"`
	RC         int    `json:"rc"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
	Project    string `json:"project"`
	CWD        string `json:"cwd"`
	Session    string `json:"session"`
	Tool       string `json:"tool"`
	Payload    string `json:"payload"`
	Event      string `json:"event"`
	Version    string `json:"blotter_version"`
}

func FromEvent(raw []byte) Hit {
	h := Hit{TS: Timestamp(time.Now()), Event: string(raw), Project: os.Getenv("CLAUDE_PROJECT_DIR")}
	var event struct {
		CWD     string          `json:"cwd"`
		Session string          `json:"session_id"`
		Tool    string          `json:"tool_name"`
		Input   json.RawMessage `json:"tool_input"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return h
	}
	h.CWD = event.CWD
	h.Session = event.Session
	h.Tool = event.Tool
	if h.Project == "" {
		h.Project = h.CWD
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(event.Input, &input) == nil {
		for _, k := range []string{"command", "file_path", "notebook_path"} {
			var text *string
			if value, ok := input[k]; ok && json.Unmarshal(value, &text) == nil && text != nil {
				h.Payload = *text
				return h
			}
		}
	}
	var compact bytes.Buffer
	if len(event.Input) > 0 && json.Compact(&compact, event.Input) == nil {
		h.Payload = compact.String()
	}
	return h
}

func Hash(text string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(text))) }
func (h *Hit) Prepare(privacy string) {
	if len(h.Stdout) > CaptureCap {
		h.Stdout = h.Stdout[:CaptureCap]
		h.Truncated = true
	}
	if len(h.Stderr) > CaptureCap {
		h.Stderr = h.Stderr[:CaptureCap]
		h.Truncated = true
	}
	if privacy == "hash" {
		h.Payload = Hash(h.Payload)
		h.Event = ""
		h.Stdout = ""
		h.Stderr = ""
	}
}

func (s *Store) Insert(ctx context.Context, h Hit) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO hits(ts,guard,harness,outcome,rc,stdout,stderr,truncated,duration_ms,project,cwd,session,tool,payload,event,blotter_version) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		h.TS, h.Guard, h.Harness, h.Outcome, h.RC, h.Stdout, h.Stderr, h.Truncated, h.DurationMS, h.Project, h.CWD, h.Session, h.Tool, h.Payload, h.Event, h.Version)
	return err
}

const hitColumns = "id,ts,guard,harness,outcome,rc,stdout,stderr,truncated,duration_ms,project,cwd,session,tool,payload,event,blotter_version"

type scanner interface{ Scan(...any) error }

func scanHit(s scanner) (h Hit, err error) {
	err = s.Scan(&h.ID, &h.TS, &h.Guard, &h.Harness, &h.Outcome, &h.RC, &h.Stdout, &h.Stderr, &h.Truncated, &h.DurationMS, &h.Project, &h.CWD, &h.Session, &h.Tool, &h.Payload, &h.Event, &h.Version)
	return
}
func (s *Store) Show(ctx context.Context, id int64) (Hit, error) {
	return scanHit(s.DB.QueryRowContext(ctx, "SELECT "+hitColumns+" FROM hits WHERE id=?", id))
}

type Filter struct {
	Guard, Project, Harness, Since string
	Outcomes                       []string
	Limit                          int
}

func (f Filter) where() (string, []any, error) {
	clauses := []string{"1=1"}
	args := []any{}
	for _, x := range []struct{ k, v string }{{"guard", f.Guard}, {"harness", f.Harness}, {"ts", f.Since}} {
		if x.v != "" {
			op := "="
			if x.k == "ts" {
				op = ">="
			}
			clauses = append(clauses, x.k+op+"?")
			args = append(args, x.v)
		}
	}
	if f.Project != "" {
		p, err := filepath.Abs(f.Project)
		if err != nil {
			return "", nil, err
		}
		p = filepath.Clean(p)
		prefix := p
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		clauses = append(clauses, "(project=? OR substr(project,1,length(?))=?)")
		args = append(args, p, prefix, prefix)
	}
	if len(f.Outcomes) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(f.Outcomes)), ",")
		clauses = append(clauses, "outcome IN ("+marks+")")
		for _, o := range f.Outcomes {
			args = append(args, o)
		}
	}
	return strings.Join(clauses, " AND "), args, nil
}
func (s *Store) List(ctx context.Context, f Filter) ([]Hit, error) {
	w, args, err := f.where()
	if err != nil {
		return nil, err
	}
	// #nosec G202 -- WHERE contains only fixed SQL fragments; all filter values are bound parameters.
	query := "SELECT " + hitColumns + " FROM hits WHERE " + w + " ORDER BY ts DESC,id DESC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	hits := []Hit{}
	for rows.Next() {
		h, e := scanHit(rows)
		if e != nil {
			return nil, e
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

type Stat struct {
	Guard   string `json:"guard,omitempty"`
	Day     string `json:"day,omitempty"`
	Block   int    `json:"block"`
	Error   int    `json:"error"`
	Ask     int    `json:"ask"`
	Pass    int    `json:"pass"`
	Canary  int    `json:"canary"`
	LastHit string `json:"last_hit"`
}

func (s *Store) Stats(ctx context.Context, f Filter, by string) ([]Stat, error) {
	guard, day := "''", "''"
	switch by {
	case "guard":
		guard = "guard"
	case "day":
		day = "substr(ts,1,10)"
	case "guard,day":
		guard = "guard"
		day = "substr(ts,1,10)"
	default:
		return nil, fmt.Errorf("invalid grouping %q", by)
	}
	w, args, err := f.where()
	if err != nil {
		return nil, err
	}
	// #nosec G202 -- Group expressions are selected from a closed enum; filter values use placeholders.
	q := `SELECT ` + guard + `,` + day + `,sum(outcome='block'),sum(outcome='error'),sum(outcome='ask'),sum(outcome='pass'),sum(outcome='canary'),max(ts) FROM hits WHERE ` + w + ` GROUP BY 1,2 ORDER BY 1,2`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Stat{}
	for rows.Next() {
		var r Stat
		if err = rows.Scan(&r.Guard, &r.Day, &r.Block, &r.Error, &r.Ask, &r.Pass, &r.Canary, &r.LastHit); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

var durationRE = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?[hdw]$`)

func Since(value string, now time.Time) (string, error) {
	if value == "" {
		return "", nil
	}
	if t, err := time.Parse("2006-01-02", value); err == nil {
		return Timestamp(t), nil
	}
	if !durationRE.MatchString(value) {
		return "", fmt.Errorf("invalid --since %q (use hours, days, weeks or YYYY-MM-DD)", value)
	}
	if len(value) > 1 {
		suffix := value[len(value)-1]
		if suffix == 'd' || suffix == 'w' {
			mult := 24
			if suffix == 'w' {
				mult *= 7
			}
			d, err := time.ParseDuration(value[:len(value)-1] + "h")
			if err == nil && d > 0 && d <= time.Duration(1<<63-1)/time.Duration(mult) {
				return Timestamp(now.Add(-d * time.Duration(mult))), nil
			}
		}
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return "", fmt.Errorf("invalid --since %q (use hours, days, weeks or YYYY-MM-DD)", value)
	}
	return Timestamp(now.Add(-d)), nil
}
func ValidOutcome(s string) bool {
	switch s {
	case "block", "error", "ask", "pass", "canary":
		return true
	}
	return false
}

type Canary struct {
	Token   string `json:"token"`
	Created string `json:"created"`
}

func (s *Store) NewCanary(ctx context.Context, token string) error {
	_, e := s.DB.ExecContext(ctx, "INSERT INTO canaries VALUES (?,?)", token, Timestamp(time.Now()))
	return e
}
func (s *Store) LatestCanary(ctx context.Context) (c Canary, err error) {
	err = s.DB.QueryRowContext(ctx, "SELECT token,created FROM canaries ORDER BY created DESC,rowid DESC LIMIT 1").Scan(&c.Token, &c.Created)
	return
}
func (s *Store) KnownCanary(ctx context.Context, token string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, "SELECT 1 FROM canaries WHERE token=?", token).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
func (s *Store) CanaryHits(ctx context.Context, token string) ([]Hit, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+hitColumns+" FROM hits WHERE outcome='canary' AND payload IN (?,?) ORDER BY ts,id", token, Hash(token))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Hit{}
	for rows.Next() {
		h, e := scanHit(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
