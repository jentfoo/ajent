package plan

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// gitTimeout bounds the working-tree capture handed to a review round. The two
// commands run concurrently, so this is the whole wait the drain goroutine takes
// between the implementation turn and the review turn.
const gitTimeout = 5 * time.Second

// GitState captures the working tree for a review round. Failures are reported
// in place rather than blocking the review.
func GitState(ctx context.Context) (status, diffStat string) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); status = runGit(ctx, "status", "--porcelain") }()
	go func() { defer wg.Done(); diffStat = runGit(ctx, "diff", "--stat") }()
	wg.Wait()
	return status, diffStat
}

// runGit returns the trimmed stdout of one git invocation, or a parenthesised
// note when it could not run.
func runGit(ctx context.Context, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return "(git " + args[0] + " unavailable: " + err.Error() + ")"
	}
	return strings.TrimRight(string(out), "\n")
}
