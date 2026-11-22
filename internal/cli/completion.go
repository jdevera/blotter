package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jdevera/blotter/internal/install"
	"github.com/jdevera/blotter/internal/store"
)

var harnesses = []string{"claude", "codex"}

// completeFlag registers a value completion; a missing flag is a programming
// error caught by the completion tests.
func completeFlag(c *cobra.Command, flag string, fn cobra.CompletionFunc) {
	if err := c.RegisterFlagCompletionFunc(flag, fn); err != nil {
		panic(err)
	}
}

func fixed(values ...string) cobra.CompletionFunc {
	return cobra.FixedCompletions(values, cobra.ShellCompDirectiveNoFileComp)
}

func dirs(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveFilterDirs
}

// commaList completes the next item of a comma-separated value, skipping
// items already given.
func commaList(values ...string) cobra.CompletionFunc {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		prefix := toComplete[:strings.LastIndex(toComplete, ",")+1]
		given := strings.Split(prefix, ",")
		out := []cobra.Completion{}
		for _, v := range values {
			if !slices.Contains(given, v) {
				out = append(out, prefix+v)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
}

// existing opens the database only if it already exists: completion must not
// create one.
func (a *app) existing() (*store.Store, bool) {
	path := store.DBPath(a.db)
	if _, err := os.Stat(path); err != nil {
		return nil, false
	}
	s, err := store.Open(context.Background(), path)
	return s, err == nil
}

func (a *app) guardNames(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	s, ok := a.existing()
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer func() { _ = s.Close() }()
	names, _ := s.Guards(context.Background())
	return names, cobra.ShellCompDirectiveNoFileComp
}

func (a *app) installedGuards(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	names, _ := install.InstalledGuards(store.DBPath(a.db))
	return names, cobra.ShellCompDirectiveNoFileComp
}

// hitIDs offers the newest hits, described so the right one can be picked.
func (a *app) hitIDs(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	s, ok := a.existing()
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer func() { _ = s.Close() }()
	hits, _ := s.List(context.Background(), store.Filter{Limit: 50})
	out := []cobra.Completion{}
	for _, h := range hits {
		out = append(out, cobra.CompletionWithDesc(fmt.Sprint(h.ID), fmt.Sprintf("%s %s %s", h.Guard, h.Outcome, h.TS)))
	}
	return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveKeepOrder
}

// scopes completes --scope for the harness given as the first argument.
func scopes(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 && args[0] == "codex" {
		return []cobra.Completion{"user", "project"}, cobra.ShellCompDirectiveNoFileComp
	}
	return []cobra.Completion{"user", "project", "local"}, cobra.ShellCompDirectiveNoFileComp
}
