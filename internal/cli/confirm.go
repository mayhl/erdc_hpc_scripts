package cli

import (
	"fmt"
	"os"
	"strings"
)

// confirm asks a question on stderr with a [y/N] tail; only an explicit y accepts.
// Callers keep their own abort messages — a couple qualify them.
// FUTURE: the one seam a MU_HEADLESS accept policy would hook.
func confirm(format string, args ...any) bool {
	fmt.Fprintf(os.Stderr, format+" [y/N] ", args...)
	var r string
	_, _ = fmt.Scanln(&r)
	return strings.ToLower(strings.TrimSpace(r)) == "y"
}
