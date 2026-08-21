package command

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/strutil"
)

// AgentJob is one /agents row.
type AgentJob struct {
	ID      string
	Status  string // queued, running, done, error, aborted
	Task    string
	Elapsed time.Duration
}

// Agents is the sub-agent manager's view /agents needs, declared here so
// pkg/command does not import pkg/subagent.
type Agents interface {
	List() []AgentJob
	Stop(id string) error // cancel one job; finished ones return an error
	StopAll() int         // cancel every in-flight job, returns how many
}

// agentsCommand lists running sub-agents or stops them. SplitCommand only splits
// the first word, so the verb is parsed here.
func agentsCommand(_ context.Context, arg string, c Console) error {
	a := c.Agents()
	if a == nil {
		c.Notify("sub-agents not available", levelWarn)
		return nil
	}
	verb, rest, _ := strings.Cut(strings.TrimSpace(arg), " ")
	rest = strings.TrimSpace(rest)

	switch verb {
	case "", "list":
		agentsList(c, a)
	case "stop":
		if rest == "" || rest == "all" {
			n := a.StopAll()
			c.Notify(fmt.Sprintf("stopped %d sub-agent(s)", n), levelInfo)
			return nil
		}
		if err := a.Stop(rest); err != nil {
			c.Notify(err.Error(), levelWarn)
		} else {
			c.Notify("sub-agent "+rest+": stopping", levelInfo)
		}
	default:
		c.Notify(fmt.Sprintf("unknown /agents verb %q; try list or stop", verb), levelWarn)
	}
	return nil
}

// agentsList prints a markdown table of every sub-agent.
func agentsList(c Console, a Agents) {
	jobs := a.List()
	var b strings.Builder
	if len(jobs) == 0 {
		c.Print("_no sub-agents_\n")
		return
	}
	b.WriteString("# Sub-agents\n\n| id | status | elapsed | task |\n")
	b.WriteString("|----|--------|---------:|------|\n")
	for _, j := range jobs {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			j.ID, j.Status, strutil.Elapsed(j.Elapsed), orDash(j.Task))
	}
	c.Print(b.String())
}

// agentsCompletion offers verbs, then job ids after stop.
func agentsCompletion(c Console) func(prefix string) []string {
	return func(prefix string) []string {
		a := c.Agents()
		if a == nil {
			return nil
		}
		verb, rest, hasSpace := strings.Cut(prefix, " ")
		switch {
		case !hasSpace:
			out := make([]string, 0, 2)
			for _, v := range []string{"list", "stop"} {
				if verb == "" || strings.HasPrefix(v, verb) {
					out = append(out, v)
				}
			}
			return out
		case verb == "stop":
			ids := []string{"all"} // stop all is offered alongside job ids
			jobs := a.List()
			for _, j := range jobs {
				ids = append(ids, j.ID)
			}
			return filterPrefix(ids, rest)
		default:
			return nil
		}
	}
}
