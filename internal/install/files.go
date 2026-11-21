package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/jdevera/blotter/internal/store"
)

type change struct {
	Path          string
	Before, After []byte
	Exists        bool
	Mode          os.FileMode
}
type reportWriter struct {
	io.Writer
	err error
}

func (w *reportWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.Writer.Write(p)
	w.err = err
	return n, err
}

func readChange(path string) (change, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return change{}, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, os.ErrNotExist) {
		parent, e := filepath.EvalSymlinks(filepath.Dir(abs))
		if e == nil {
			real = filepath.Join(parent, filepath.Base(abs))
		} else {
			real = abs
		}
	} else if err != nil {
		return change{}, err
	}
	c := change{Path: real, Mode: 0600}
	b, err := os.ReadFile(real)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return c, err
	}
	c.Before = b
	c.After = bytes.Clone(b)
	c.Exists = true
	c.Mode = info.Mode().Perm()
	return c, nil
}
func atomicWrite(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".blotter-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
func replace(c change) error {
	current, err := os.ReadFile(c.Path)
	if !c.Exists && errors.Is(err, os.ErrNotExist) {
		err = nil
	} else if err == nil && (!c.Exists || !bytes.Equal(current, c.Before)) {
		return fmt.Errorf("%s changed while blotter was preparing the update", c.Path)
	}
	if err != nil {
		return err
	}
	return atomicWrite(c.Path, c.After, c.Mode)
}
func rollback(c change) error {
	current, err := os.ReadFile(c.Path)
	if err != nil {
		return err
	}
	if bytes.Equal(current, c.Before) && c.Exists {
		return nil
	}
	if !bytes.Equal(current, c.After) {
		return fmt.Errorf("cannot roll back %s: it was edited after blotter wrote it", c.Path)
	}
	if !c.Exists {
		return os.Remove(c.Path)
	}
	return atomicWrite(c.Path, c.Before, c.Mode)
}
func lockInstall() (*os.File, error) {
	if err := os.MkdirAll(store.StateDir(), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(store.StateDir(), "install.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { // #nosec G115 -- OS descriptors fit in signed int.
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
func unlock(f *os.File) { // #nosec G115 -- OS descriptors fit in signed int.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

type backupMeta struct {
	Target  string `json:"target"`
	Harness string `json:"harness"`
	Scope   string `json:"scope"`
	Version string `json:"blotter_version"`
	Created string `json:"created"`
}

func backupDir(target string) string {
	sum := sha256.Sum256([]byte(target))
	return filepath.Join(store.DataDir(), "backups", fmt.Sprintf("%x", sum[:8]))
}
func backup(c change, o Options) error {
	if !c.Exists {
		return nil
	}
	dir := backupDir(c.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	meta := backupMeta{c.Path, o.Harness, o.Scope, o.Version, store.Timestamp(time.Now())}
	text, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	// Each timestamp owns its metadata; older snapshots retain their provenance.
	if err = atomicWrite(filepath.Join(dir, stamp), c.Before, 0600); err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(dir, stamp+".meta.json"), append(text, '\n'), 0600); err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(dir, "meta.json"), append(text, '\n'), 0600); err != nil {
		return err
	}
	files, err := backupFiles(dir)
	if err != nil {
		return err
	}
	for _, file := range files[min(10, len(files)):] {
		if err = os.Remove(filepath.Join(dir, file)); err != nil {
			return err
		}
		if err = os.Remove(filepath.Join(dir, file+".meta.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
func backupFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := []string{}
	for _, e := range entries {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".json") {
			files = append(files, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	return files, nil
}
func newestBackup(target string) ([]byte, error) {
	dir := backupDir(target)
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var m backupMeta
	if err = json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Target != target {
		return nil, fmt.Errorf("backup target mismatch for %s", target)
	}
	files, err := backupFiles(dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no backups for %s", target)
	}
	return os.ReadFile(filepath.Join(dir, files[0]))
}
func ListBackups(out io.Writer) error {
	dir := filepath.Join(store.DataDir(), "backups")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		folder := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(filepath.Join(folder, "meta.json"))
		if err != nil {
			return err
		}
		var m backupMeta
		if err = json.Unmarshal(b, &m); err != nil {
			return err
		}
		files, err := backupFiles(folder)
		if err != nil {
			return err
		}
		for _, f := range files {
			if _, err = fmt.Fprintf(out, "%s  %s  %s\n", m.Target, m.Harness, filepath.Join(folder, f)); err != nil {
				return err
			}
		}
	}
	return nil
}
func printDiff(out io.Writer, c change) error {
	status := "update"
	if !c.Exists {
		status = "create"
	}
	if bytes.Equal(c.Before, c.After) {
		status = "unchanged"
	}
	if status != "unchanged" {
		text, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: difflib.SplitLines(string(c.Before)), B: difflib.SplitLines(string(c.After)), FromFile: c.Path, ToFile: c.Path, Context: 3})
		if err != nil {
			return err
		}
		if _, err = io.WriteString(out, text); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "%s %s\n", status, c.Path)
	return err
}

type Manifest struct {
	ID                                 int64
	Harness, Scope, Target, Event      string
	GroupIndex, HandlerIndex           int
	Guard, Original, Wrapped, TrustKey string
	PriorTrust                         *string
	InstalledAt, Version               string
	FailOpen                           bool
	SourceTarget                       string
}

const manifestCols = "id,harness,scope,target,event,group_index,handler_index,guard,original,wrapped,trust_key,prior_trust,installed_at,blotter_version,fail_open,source_target"

func manifests(db *sql.DB) ([]Manifest, error) {
	rows, err := db.Query("SELECT " + manifestCols + " FROM installs ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Manifest{}
	for rows.Next() {
		var m Manifest
		if err = rows.Scan(&m.ID, &m.Harness, &m.Scope, &m.Target, &m.Event, &m.GroupIndex, &m.HandlerIndex, &m.Guard, &m.Original, &m.Wrapped, &m.TrustKey, &m.PriorTrust, &m.InstalledAt, &m.Version, &m.FailOpen, &m.SourceTarget); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func readManifests(path string) ([]Manifest, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{"mode": []string{"ro"}, "_pragma": []string{"busy_timeout(250)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	return manifests(db)
}
func saveManifests(tx *sql.Tx, all []Manifest) error {
	if _, err := tx.Exec("DELETE FROM installs"); err != nil {
		return err
	}
	for _, m := range all {
		_, err := tx.Exec(`INSERT INTO installs(harness,scope,target,event,group_index,handler_index,guard,original,wrapped,trust_key,prior_trust,installed_at,blotter_version,fail_open,source_target) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, m.Harness, m.Scope, m.Target, m.Event, m.GroupIndex, m.HandlerIndex, m.Guard, m.Original, m.Wrapped, m.TrustKey, m.PriorTrust, m.InstalledAt, m.Version, m.FailOpen, m.SourceTarget)
		if err != nil {
			return err
		}
	}
	return nil
}

type pending struct {
	ID, DB  string
	Changes []change
}

func journalPath() string { return filepath.Join(store.StateDir(), "install.pending.json") }
func recoverPending() error {
	b, err := os.ReadFile(journalPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var p pending
	if err = json.Unmarshal(b, &p); err != nil {
		return err
	}
	s, err := store.Open(context.Background(), p.DB)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	if _, err = s.DB.Exec("CREATE TABLE IF NOT EXISTS install_operations(id TEXT PRIMARY KEY)"); err != nil {
		return err
	}
	var n int
	err = s.DB.QueryRow("SELECT 1 FROM install_operations WHERE id=?", p.ID).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if n != 1 {
		for i := len(p.Changes) - 1; i >= 0; i-- {
			c := p.Changes[i]
			current, e := os.ReadFile(c.Path)
			if (!c.Exists && errors.Is(e, os.ErrNotExist)) || bytes.Equal(current, c.Before) {
				continue
			}
			if e != nil {
				return e
			}
			if e = rollback(c); e != nil {
				return e
			}
		}
	}
	return os.Remove(journalPath())
}
func commitChanges(changes []change, all []Manifest, o Options) error {
	active := []change{}
	for _, c := range changes {
		if !bytes.Equal(c.Before, c.After) {
			active = append(active, c)
		}
	}
	if len(active) == 0 {
		return nil
	}
	s, err := store.Open(context.Background(), o.DB)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	if _, err = s.DB.Exec("CREATE TABLE IF NOT EXISTS install_operations(id TEXT PRIMARY KEY)"); err != nil {
		return err
	}
	for _, c := range active {
		if err = backup(c, o); err != nil {
			return err
		}
	}
	p := pending{ID: time.Now().UTC().Format("20060102T150405.000000000"), DB: o.DB, Changes: active}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err = atomicWrite(journalPath(), b, 0600); err != nil {
		return err
	}
	written := []change{}
	fail := func(cause error) error {
		for i := len(written) - 1; i >= 0; i-- {
			if e := rollback(written[i]); e != nil {
				return errors.Join(cause, e)
			}
		}
		if e := os.Remove(journalPath()); e != nil {
			return errors.Join(cause, e)
		}
		return cause
	}
	for _, c := range active {
		if err = replace(c); err != nil {
			// A rename may have succeeded before a directory fsync failed.
			if current, e := os.ReadFile(c.Path); e == nil && bytes.Equal(current, c.After) {
				written = append(written, c)
			}
			return fail(err)
		}
		written = append(written, c)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return fail(err)
	}
	if err = saveManifests(tx, all); err == nil {
		_, err = tx.Exec("INSERT INTO install_operations VALUES (?)", p.ID)
	}
	if err != nil {
		_ = tx.Rollback()
		return fail(err)
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	return os.Remove(journalPath())
}
