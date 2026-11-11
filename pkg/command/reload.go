package command

import "context"

// reloadCommand re-reads the configuration files. The host owns every progress
// notice, since it knows which live objects a re-read actually moved.
func reloadCommand(ctx context.Context, _ string, c Console) error {
	if err := c.Reload(ctx); err != nil {
		c.Notify(err.Error(), levelError)
	}
	return nil
}
