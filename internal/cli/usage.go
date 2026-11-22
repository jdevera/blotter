package cli

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// usageError is a mistake in how blotter was invoked. Run reports it with the
// command's usage line, unlike a failure while the command runs.
type usageError struct{ error }

func usagef(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

// oneOf checks that value, named by label (a flag such as "--scope" or an
// argument such as "harness"), is one of valid.
func oneOf(label, value string, valid ...string) error {
	if slices.Contains(valid, value) {
		return nil
	}
	if value == "" {
		return usagef("%s is required: %s", label, alternatives(valid))
	}
	return usagef("%s must be %s, not %q", label, alternatives(valid), value)
}

func alternatives(valid []string) string {
	if len(valid) == 1 {
		return valid[0]
	}
	return strings.Join(valid[:len(valid)-1], ", ") + " or " + valid[len(valid)-1]
}

// exactlyOne requires a single positional argument, explained by what.
func exactlyOne(what string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return usageError{fmt.Errorf("%s", what)}
		}
		return nil
	}
}

var parseNoise = regexp.MustCompile(`strconv\.Parse\w+: parsing "[^"]*": (invalid syntax)?`)

// flagError marks flag parsing failures as usage errors and replaces the Go
// parser's wording in messages such as `invalid argument "x" for "--limit"`.
func flagError(_ *cobra.Command, err error) error {
	msg := parseNoise.ReplaceAllStringFunc(err.Error(), func(m string) string {
		if strings.HasSuffix(m, "invalid syntax") {
			return "not a number"
		}
		return ""
	})
	return usageError{fmt.Errorf("%s", msg)}
}
