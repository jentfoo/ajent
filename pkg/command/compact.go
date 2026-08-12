package command

import "context"

// compactCommand reduces the session's context toward the compaction threshold.
// An optional argument becomes the summariser's focus instruction; refused while a
// turn is streaming.
func compactCommand(ctx context.Context, arg string, c Console) error {
	return c.Compact(ctx, arg)
}
