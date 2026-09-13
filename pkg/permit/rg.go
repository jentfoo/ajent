package permit

import (
	"slices"
	"strings"
)

// rgReadOnly reports whether an rg invocation is verifiably free of exec or
// write vectors. Only --pre/--hostname-bin (which run a command) disqualify;
// there are no short forms for either, so exact and attached spellings suffice.
func rgReadOnly(tokens []string) bool {
	return !slices.ContainsFunc(tokens, func(t string) bool {
		return t == "--pre" || strings.HasPrefix(t, "--pre=") ||
			t == "--hostname-bin" || strings.HasPrefix(t, "--hostname-bin=")
	})
}
