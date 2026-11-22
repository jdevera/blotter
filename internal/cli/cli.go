package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jdevera/blotter/internal/guard"
	"github.com/jdevera/blotter/internal/install"
	"github.com/jdevera/blotter/internal/store"
)

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

type app struct {
	db, version string
	in          io.Reader
	out, err    io.Writer
}

func Run(args []string, version string, in io.Reader, out, stderr io.Writer) (code int) {
	a := &app{version: version, in: in, out: out, err: stderr}
	wrapping := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--db" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "--db=") {
			continue
		}
		wrapping = args[i] == "wrap"
		break
	}
	guardName := "<unknown>"
	for i, arg := range args {
		if arg == "--" {
			break // the guard's own arguments follow
		}
		if arg == "--guard" && i+1 < len(args) {
			guardName = args[i+1]
		}
		if strings.HasPrefix(arg, "--guard=") {
			guardName = strings.TrimPrefix(arg, "--guard=")
		}
	}
	defer func() {
		if p := recover(); p != nil {
			store.Diagnostic(store.KindPanic, guardName, fmt.Errorf("recovered panic: %v", p))
			code = 1
			if wrapping {
				_, _ = fmt.Fprintf(stderr, "BLOCKED: blotter could not run guard %s: internal failure\n", guardName)
				code = 2
			}
		}
	}()
	root := &cobra.Command{Use: "blotter", Short: "Record agent guard blocks and failures locally", Version: version, SilenceUsage: true, SilenceErrors: true, CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true}}
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(stderr)
	root.SetArgs(args)
	root.PersistentFlags().StringVar(&a.db, "db", "", "database path (or BLOTTER_DB)")
	root.AddCommand(a.wrap(), a.record(), a.ls(), a.show(), a.stats(), a.canary(), a.installCommand(false), a.installCommand(true), a.backups(), a.doctor())
	if err := root.Execute(); err != nil {
		var exit statusError
		if errors.As(err, &exit) {
			return int(exit)
		}
		if wrapping {
			_, _ = fmt.Fprintf(stderr, "BLOCKED: blotter could not run guard %s: %v\n", guardName, err)
			return 2
		}
		_, _ = fmt.Fprintf(stderr, "blotter: %v\n", err)
		return 1
	}
	return 0
}
func (a *app) open() (*store.Store, error) {
	return store.Open(context.Background(), store.DBPath(a.db))
}
func (a *app) json(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) wrap() *cobra.Command {
	var harness, name, shell, log, privacy string
	var shellWords bool
	c := &cobra.Command{Use: "wrap [flags] -- program [args...]", Short: "Run a guard and preserve its output and exit status", RunE: func(c *cobra.Command, args []string) error {
		// A harness that stops reading must not kill blotter before it logs the
		// hit: with SIGPIPE handled, writes return EPIPE instead. Unlike an
		// ignored signal, a handled one is reset to default for the guard.
		signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
		if harness != "claude" && harness != "codex" {
			return fmt.Errorf("--harness must be claude or codex")
		}
		if name == "" {
			return fmt.Errorf("--guard is required")
		}
		if privacy != "full" && privacy != "hash" {
			return fmt.Errorf("--record must be full or hash")
		}
		if c.Flags().Changed("sh") {
			if len(args) != 0 || shellWords {
				return fmt.Errorf("--sh cannot be combined with a program or --shell-words")
			}
		} else if c.Flags().ArgsLenAtDash() != 0 || len(args) == 0 || args[0] == "" {
			return fmt.Errorf("supply a guard program after --, or use --sh for a shell command")
		}
		logs := map[string]bool{}
		for _, s := range strings.Split(log, ",") {
			if s == "all" {
				logs["all"] = true
				continue
			}
			if !store.ValidOutcome(s) {
				return fmt.Errorf("invalid --log outcome %q", s)
			}
			logs[s] = true
		}
		input, err := io.ReadAll(io.LimitReader(a.in, (16<<20)+1))
		if err != nil {
			return err
		}
		if len(input) > 16<<20 {
			return fmt.Errorf("stdin exceeds 16 MiB")
		}
		h := store.FromEvent(input)
		h.Guard = name
		h.Harness = harness
		h.Version = a.version
		if strings.Contains(string(input), "BLOTTER-CANARY-") {
			token := a.findCanary(input)
			if token != "" {
				h.Outcome = "canary"
				h.RC = 2
				h.Payload = token
				h.Stderr = fmt.Sprintf("BLOCKED by blotter canary %s: the guard wiring works. Tell the user this canary was blocked.\n", token)
				_, _ = io.WriteString(a.err, h.Stderr)
				a.logHit(h, privacy)
				return statusError(2)
			}
		}
		switch {
		case c.Flags().Changed("sh"):
			args = []string{"/bin/sh", "-c", shell}
		case shellWords:
			args, err = guard.ExpandWords(args)
		}
		if err != nil {
			return err
		}
		result := guard.Run(args, input)
		if result.LaunchErr != nil {
			h.Outcome = "error"
			h.RC = 2
			h.DurationMS = result.Duration.Milliseconds()
			h.Stderr = fmt.Sprintf("BLOCKED: blotter could not run guard %s: %v\n", name, result.LaunchErr)
			_, _ = io.WriteString(a.err, h.Stderr)
			a.logHit(h, privacy)
			return statusError(2)
		}
		return a.passthrough(result, h, logs, privacy)
	}}
	f := c.Flags()
	f.StringVar(&harness, "harness", "", "claude or codex")
	f.StringVar(&name, "guard", "", "guard name")
	f.BoolVar(&shellWords, "shell-words", false, "expand saved shell words after -- (used by installed shell-form hooks)")
	f.StringVar(&shell, "sh", "", "command to run with /bin/sh -c")
	f.StringVar(&log, "log", "block,error", "outcomes to log, or all")
	f.StringVar(&privacy, "record", "full", "full or hash payload recording")
	return c
}
func (a *app) passthrough(result guard.Result, h store.Hit, logs map[string]bool, privacy string) (err error) {
	err = statusError(result.RC)
	defer func() {
		if p := recover(); p != nil {
			store.Diagnostic(store.KindPanic, h.Guard, fmt.Errorf("finalizing guard %s: recovered panic: %v", h.Guard, p))
		}
	}()
	if _, err := a.out.Write(result.Stdout); err != nil {
		store.Diagnostic(store.KindPassthrough, h.Guard, fmt.Errorf("guard %s stdout passthrough: %w", h.Guard, err))
	}
	if _, err := a.err.Write(result.Stderr); err != nil {
		store.Diagnostic(store.KindPassthrough, h.Guard, fmt.Errorf("guard %s stderr passthrough: %w", h.Guard, err))
	}
	if result.CaptureErr != nil {
		kind := store.KindCapture
		if errors.Is(result.CaptureErr, exec.ErrWaitDelay) {
			kind = store.KindBackground
		}
		store.Diagnostic(kind, h.Guard, fmt.Errorf("guard %s pipe capture: %w", h.Guard, result.CaptureErr))
	}
	h.RC = result.RC
	h.Outcome = guard.Classify(h.Harness, result.RC, result.Stdout)
	h.Stdout = string(result.Stdout)
	h.Stderr = string(result.Stderr)
	h.DurationMS = result.Duration.Milliseconds()
	if logs["all"] || logs[h.Outcome] {
		a.logHit(h, privacy)
	}
	return statusError(result.RC)
}
func (a *app) logHit(h store.Hit, privacy string) {
	defer func() {
		if p := recover(); p != nil {
			store.Diagnostic(store.KindPanic, h.Guard, fmt.Errorf("logging panic for guard %s: %v", h.Guard, p))
		}
	}()
	h.Prepare(privacy)
	if h.Truncated {
		store.Diagnostic(store.KindTruncated, h.Guard, fmt.Errorf("guard %s capture truncated at %d bytes per stored stream", h.Guard, store.CaptureCap))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	s, err := store.Open(ctx, store.DBPath(a.db))
	if err == nil {
		defer func() { _ = s.Close() }()
		err = s.Insert(ctx, h)
	}
	if err != nil {
		store.Diagnostic(store.KindNotLogged, h.Guard, fmt.Errorf("guard %s logging: %w", h.Guard, err))
	}
}

var tokenRE = regexp.MustCompile(`BLOTTER-CANARY-[0-9a-f]{8}\b`)

func (a *app) findCanary(input []byte) string {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	s, err := store.Open(ctx, store.DBPath(a.db))
	if err != nil {
		store.Diagnostic(store.KindCanary, "", fmt.Errorf("canary lookup: %w", err))
		return ""
	}
	defer func() { _ = s.Close() }()
	for _, t := range tokenRE.FindAll(input, -1) {
		known, err := s.KnownCanary(ctx, string(t))
		if err != nil {
			store.Diagnostic(store.KindCanary, "", fmt.Errorf("canary lookup: %w", err))
			return ""
		}
		if known {
			return string(t)
		}
	}
	return ""
}

func (a *app) record() *cobra.Command {
	var name, harness, outcome, payload, project, session, event string
	var rc int
	c := &cobra.Command{Use: "record", Short: "Record an outcome from a script or another harness", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if name == "" || harness == "" || !store.ValidOutcome(outcome) {
			return fmt.Errorf("--guard, --harness and a valid --outcome are required")
		}
		if rc < 0 || rc > 255 {
			return fmt.Errorf("--rc must be between 0 and 255")
		}
		var raw []byte
		var err error
		if event == "-" {
			raw, err = io.ReadAll(io.LimitReader(a.in, (16<<20)+1))
		} else if event != "" {
			raw, err = os.ReadFile(event)
		}
		if err != nil {
			return err
		}
		if len(raw) > 16<<20 {
			return fmt.Errorf("event exceeds 16 MiB")
		}
		h := store.FromEvent(raw)
		h.Guard = name
		h.Harness = harness
		h.Outcome = outcome
		h.RC = rc
		h.Version = a.version
		if c.Flags().Changed("payload") {
			h.Payload = payload
		}
		if c.Flags().Changed("project") {
			h.Project, err = filepath.Abs(project)
			if err != nil {
				return err
			}
		}
		if c.Flags().Changed("session") {
			h.Session = session
		}
		s, err := a.open()
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		return s.Insert(context.Background(), h)
	}}
	f := c.Flags()
	f.StringVar(&name, "guard", "", "guard name")
	f.StringVar(&harness, "harness", "", "harness name")
	f.StringVar(&outcome, "outcome", "", "block, error, ask, pass or canary")
	f.StringVar(&payload, "payload", "", "recorded payload")
	f.StringVar(&project, "project", "", "project path")
	f.StringVar(&session, "session", "", "session ID")
	f.IntVar(&rc, "rc", 0, "return code")
	f.StringVar(&event, "event-file", "", "event JSON file, or - for stdin")
	return c
}
func filters(c *cobra.Command, f *store.Filter, since *string) {
	p := c.Flags()
	p.StringVar(&f.Guard, "guard", "", "guard name")
	p.StringVar(&f.Project, "project", "", "project directory (includes descendants)")
	p.StringVar(since, "since", "", "duration or UTC date")
}
func (a *app) ls() *cobra.Command {
	var f store.Filter
	var since, outcomes string
	var asJSON bool
	c := &cobra.Command{Use: "ls", Short: "List newest guard hits", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		var err error
		f.Since, err = store.Since(since, time.Now())
		if err != nil {
			return err
		}
		if f.Limit < 1 {
			return fmt.Errorf("--limit must be positive")
		}
		if outcomes != "" {
			f.Outcomes = strings.Split(outcomes, ",")
			for _, o := range f.Outcomes {
				if !store.ValidOutcome(o) {
					return fmt.Errorf("invalid outcome %q", o)
				}
			}
		}
		s, err := a.open()
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		rows, err := s.List(context.Background(), f)
		if err != nil {
			return err
		}
		if asJSON {
			return a.json(rows)
		}
		width := 120
		if file, ok := a.out.(*os.File); ok {
			if w, _, err := term.GetSize(int(file.Fd())); err == nil { // #nosec G115 -- OS descriptors fit in signed int.
				width = w
			}
		}
		// The id leads each row so it can be passed to show.
		idWidth := 0
		for _, h := range rows {
			idWidth = max(idWidth, len(strconv.FormatInt(h.ID, 10)))
		}
		for _, h := range rows {
			prefix := fmt.Sprintf("%*d  %s  %-20s %-6s %-16s ", idWidth, h.ID, h.TS, clean(h.Guard), h.Outcome, clean(filepath.Base(h.Project)))
			space := max(0, width-len([]rune(prefix)))
			payload := []rune(clean(h.Payload))
			if len(payload) > space {
				if space > 0 {
					payload = append(payload[:space-1], '…')
				} else {
					payload = nil
				}
			}
			_, _ = fmt.Fprintln(a.out, prefix+string(payload))
		}
		return nil
	}}
	filters(c, &f, &since)
	p := c.Flags()
	p.StringVar(&f.Harness, "harness", "", "harness name")
	p.StringVar(&outcomes, "outcome", "", "comma-separated outcomes")
	p.IntVar(&f.Limit, "limit", 50, "maximum rows")
	p.BoolVar(&asJSON, "json", false, "print full rows as JSON")
	return c
}
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 32 || r == 127 {
			return '�'
		}
		return r
	}, s)
}
func (a *app) show() *cobra.Command {
	return &cobra.Command{Use: "show <id>", Short: "Show one complete hit", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		s, err := a.open()
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		h, err := s.Show(context.Background(), id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no hit with id %d", id)
		}
		if err != nil {
			return err
		}
		return a.json(h)
	}}
}
func (a *app) stats() *cobra.Command {
	var f store.Filter
	var since, by string
	var asJSON bool
	c := &cobra.Command{Use: "stats", Short: "Count outcomes by guard and/or UTC day", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		var err error
		f.Since, err = store.Since(since, time.Now())
		if err != nil {
			return err
		}
		s, err := a.open()
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		rows, err := s.Stats(context.Background(), f, by)
		if err != nil {
			return err
		}
		lastRows := rows
		if by != "guard" {
			lastRows, err = s.Stats(context.Background(), f, "guard")
			if err != nil {
				return err
			}
		}
		last := map[string]string{}
		for _, r := range lastRows {
			last[r.Guard] = r.LastHit
		}
		if asJSON {
			return a.json(struct {
				Groups         []store.Stat      `json:"groups"`
				LastHitByGuard map[string]string `json:"last_hit_by_guard"`
			}{rows, last})
		}
		for _, r := range rows {
			_, _ = fmt.Fprintf(a.out, "%s %s  block=%d error=%d ask=%d pass=%d canary=%d last=%s\n", r.Guard, r.Day, r.Block, r.Error, r.Ask, r.Pass, r.Canary, r.LastHit)
		}
		if by != "guard" {
			for _, r := range lastRows {
				_, _ = fmt.Fprintf(a.out, "%s last hit %s\n", r.Guard, r.LastHit)
			}
		}
		return nil
	}}
	filters(c, &f, &since)
	c.Flags().StringVar(&by, "by", "guard", "guard, day or guard,day")
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return c
}
func (a *app) canary() *cobra.Command {
	c := &cobra.Command{Use: "canary", Short: "Create a token that tests hook wiring", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		s, err := a.open()
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		b := make([]byte, 4)
		if _, err = rand.Read(b); err != nil {
			return err
		}
		token := "BLOTTER-CANARY-" + hex.EncodeToString(b)
		if err = s.NewCanary(context.Background(), token); err != nil {
			return err
		}
		_, err = fmt.Fprintf(a.out, "Run `echo %s` and tell me what happens.\n", token)
		return err
	}}
	c.AddCommand(&cobra.Command{Use: "check [token]", Short: "Show which wrapped hooks caught a canary", Args: cobra.MaximumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		s, err := a.open()
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()
		token := ""
		if len(args) > 0 {
			token = args[0]
		} else {
			canary, e := s.LatestCanary(context.Background())
			if errors.Is(e, sql.ErrNoRows) {
				_, _ = fmt.Fprintln(a.out, "No canary has been created.")
				return statusError(1)
			}
			if e != nil {
				return e
			}
			token = canary.Token
		}
		hits, err := s.CanaryHits(context.Background(), token)
		if err != nil {
			return err
		}
		for _, h := range hits {
			_, _ = fmt.Fprintf(a.out, "%s %s %s %s\n", h.TS, h.Harness, h.Guard, token)
		}
		if len(hits) == 0 {
			_, _ = fmt.Fprintln(a.out, token+" has not been caught.")
			return statusError(1)
		}
		return nil
	}})
	return c
}

func (a *app) installCommand(uninstall bool) *cobra.Command {
	var o install.Options
	name, short := "install", "Wrap PreToolUse command hooks"
	if uninstall {
		name = "uninstall"
		short = "Restore original hook handlers"
	}
	c := &cobra.Command{Use: name + " claude|codex", Short: short, Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		o.Harness = args[0]
		o.Uninstall = uninstall
		o.DB = store.DBPath(a.db)
		if a.db != "" {
			var err error
			o.WrapDB, err = filepath.Abs(a.db)
			if err != nil {
				return err
			}
		}
		o.Version = a.version
		o.Out = a.out
		return install.Apply(o)
	}}
	f := c.Flags()
	f.StringVar(&o.Scope, "scope", "user", "user, project or local")
	f.StringVar(&o.Dir, "dir", ".", "project directory")
	f.BoolVar(&o.DryRun, "dry-run", false, "print diffs without writing")
	if uninstall {
		f.StringArrayVar(&o.Only, "only", nil, "match guard name substring (repeatable)")
		f.BoolVar(&o.Restore, "restore", false, "restore newest backup of this exact target")
	} else {
		f.StringArrayVar(&o.Only, "only", nil, "match command substring (repeatable)")
		f.StringArrayVar(&o.Except, "except", nil, "exclude command substring (repeatable)")
		f.StringArrayVar(&o.FailOpen, "fail-open", nil, "wrap without adding onFailure block (repeatable)")
		f.StringVar(&o.Guard, "guard", "", "override name for a single --only match")
		f.IntVar(&o.Timeout, "timeout", 0, "explicit timeout in seconds")
		f.StringVar(&o.Record, "record", "full", "full or hash recording")
		f.BoolVar(&o.NoFailClosed, "no-fail-closed", false, "do not add Claude onFailure block")
		f.BoolVar(&o.List, "list", false, "list handlers without writing")
	}
	return c
}
func (a *app) backups() *cobra.Command {
	c := &cobra.Command{Use: "backups", Short: "Inspect settings backups"}
	c.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error { return install.ListBackups(a.out) }})
	return c
}
func (a *app) doctor() *cobra.Command {
	var dir string
	c := &cobra.Command{Use: "doctor", Short: "Check storage, hooks, trust and canaries", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		ok, err := install.Doctor(store.DBPath(a.db), dir, a.out)
		if err != nil {
			return err
		}
		if !ok {
			return statusError(1)
		}
		return nil
	}}
	c.Flags().StringVar(&dir, "dir", ".", "project directory to inspect")
	return c
}
