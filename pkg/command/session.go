package command

import (
	"context"
	"strings"
)

// sessionCommand reports the session name with no argument, or sets it. A
// malformed or already-taken name is a notice, not a failed command.
func sessionCommand(_ context.Context, arg string, c Console) error {
	name := strings.TrimSpace(arg)
	if name == "" {
		if cur := c.SessionName(); cur != "" {
			c.Notify("session name: "+cur, levelInfo)
		} else {
			c.Notify("this session is unnamed; /session <name> names it", levelInfo)
		}
		return nil
	}
	if err := c.SetSessionName(name); err != nil {
		c.Notify(err.Error(), levelWarn)
		return nil
	}
	c.Notify("session named "+name+"; resume it with `ajent --resume "+name+"`", levelInfo)
	return nil
}
