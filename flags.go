package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/pflag"

	"github.com/jentfoo/ajent/pkg/session"
)

// Exit codes a script can branch on. They are the same for text and json output.
const (
	exitOK    = 0 // the turn completed and produced a final answer
	exitUsage = 1 // bad flags, unknown model, or any setup failure before the turn
	exitTurn  = 2 // the turn itself failed, was interrupted, or produced nothing
)

// Output shapes for a one-shot run.
const (
	outputText = "text"
	outputJSON = "json"
)

// headlessFlagNames are the flags that only mean something alongside --prompt.
var headlessFlagNames = []string{"output", "allow-all", "read-only", "allow-tools", "deny-tools", "stats"}

// cliFlags is one parsed command line. The headless fields apply only when
// prompt is set.
type cliFlags struct {
	model        string
	render       string
	cont         bool
	version      bool   // --version: print the build version and exit
	update       bool   // --update: reinstall ajent from @latest in the foreground
	resume       bool   // --resume was given, with or without an id
	resumeID     string // the id or name from --resume <id|name>
	sessionName  string // --session <name>: resume that name, creating it when new
	sessionGiven bool   // --session was given, so an empty name is a usage error

	deleteTarget  string // --delete <name|id>: the session to remove from disk
	deleteGiven   bool   // --delete was given, so an empty target is a usage error
	deleteOld     bool   // --delete-old: sweep unnamed sessions unused past the cutoff
	deleteOldDays int    // days from --delete-old <days>, 0 when the default applies

	prompt     string
	output     string
	stats      bool
	allowAll   bool
	readOnly   bool
	allowTools []string
	denyTools  []string

	args     []string // positional arguments, joined into a bootstrap prompt
	headless []string // headless flag names actually given, for validation
}

// parseFlags parses argv (without the program name) into the command line. It
// reports pflag.ErrHelp when usage was requested and printed.
func parseFlags(argv []string) (cliFlags, error) {
	var f cliFlags
	// --resume and --delete-old take an optional value, which no flag package can
	// express, so they are lifted out of argv before parsing.
	f.resume, f.resumeID, argv = extractResume(argv)
	var days string
	f.deleteOld, days, argv = extractDeleteOld(argv)
	if f.deleteOld {
		f.deleteOldDays = defaultStaleDays // a bare --delete-old means the default window
	}
	if days != "" {
		n, cerr := strconv.Atoi(days)
		if cerr != nil {
			return f, fmt.Errorf("--delete-old takes a number of days, not %q", days)
		}
		f.deleteOldDays = n
	}

	fs := pflag.NewFlagSet("ajent", pflag.ContinueOnError)
	fs.StringVarP(&f.model, "model", "m", "", "initial model to use")
	fs.StringVar(&f.render, "render", "auto",
		"paint mode: auto, inline (terminal scrollback, unsupported under tmux or screen), "+
			"alt (own scrollback), plain")
	fs.BoolVar(&f.cont, "continue", false, "resume the most recent session automatically")
	fs.StringVar(&f.sessionName, "session", "", "resume the session with this name, creating it if it does not exist")
	fs.StringVar(&f.deleteTarget, "delete", "", "delete the saved session with this name or id, then exit")
	fs.BoolVarP(&f.version, "version", "v", false, "print version and exit")
	fs.BoolVar(&f.update, "update", false, "reinstall ajent from @latest then exit")
	fs.StringVarP(&f.prompt, "prompt", "p", "",
		"run one turn non-interactively from this prompt, print the result and exit")
	fs.StringVarP(&f.output, "output", "o", outputText,
		"one-shot output shape: text (the final answer) or json (one event per line)")
	fs.BoolVar(&f.allowAll, "allow-all", false, "one-shot: offer every tool, bash included")
	fs.BoolVar(&f.readOnly, "read-only", false, "one-shot: offer only read-only tools")
	fs.StringSliceVar(&f.allowTools, "allow-tools", nil, "one-shot: extra tool names to offer")
	fs.StringSliceVar(&f.denyTools, "deny-tools", nil, "one-shot: tool names to withhold")
	fs.BoolVar(&f.stats, "stats", false,
		"one-shot: print a tool and token summary to stderr (or a json summary line) when the run ends")

	fs.Usage = func() {
		writeUsage(os.Stderr, fs.FlagUsages())
	}

	if err := fs.Parse(argv); err != nil {
		return f, err
	}
	f.args = fs.Args()
	f.sessionGiven = fs.Changed("session")
	f.sessionName = strings.TrimSpace(f.sessionName)
	f.deleteGiven = fs.Changed("delete")
	f.deleteTarget = strings.TrimSpace(f.deleteTarget)
	for _, name := range headlessFlagNames {
		if fs.Changed(name) {
			f.headless = append(f.headless, "--"+name)
		}
	}
	return f, nil
}

// writeUsage writes the help text: the build version first, then the usage line,
// flag list and trailing --resume note.
func writeUsage(out io.Writer, flagUsages string) {
	printVersion(out)
	_, _ = fmt.Fprintf(out, "usage of %s:\n", os.Args[0])
	_, _ = fmt.Fprint(out, flagUsages)
	_, _ = fmt.Fprintln(out, "      --resume [id|name]      "+
		"list saved sessions and resume one; with an id or name, resume that session directly")
	_, _ = fmt.Fprintf(out, "      --delete-old [days]     "+
		"delete unnamed sessions unused for %d days, or the given number of days\n", defaultStaleDays)
}

// validate reports the first usage error in the parsed command line.
func (f cliFlags) validate() error {
	// the delete flags remove sessions and exit, so nothing that configures a run
	// belongs beside them
	if f.deleteGiven || f.deleteOld {
		switch {
		case f.deleteGiven && f.deleteOld:
			return errors.New("--delete and --delete-old are mutually exclusive")
		case f.deleteGiven && f.deleteTarget == "":
			return errors.New("--delete needs a session name or id")
		case f.deleteOld && f.deleteOldDays < 1:
			// a cutoff at or past now would sweep every unnamed session
			return errors.New("--delete-old needs a positive number of days")
		case f.deleteOld && len(f.args) > 0:
			// only a day count follows --delete-old, so a leftover positional is one
			// that did not parse as digits
			return fmt.Errorf("--delete-old takes a number of days, not %q", f.args[0])
		case f.resume || f.cont || f.sessionGiven || f.prompt != "" || len(f.args) > 0:
			return errors.New("--delete and --delete-old cannot be combined with a session or prompt flag")
		}
		return nil
	}
	// --session already means create-or-resume, so another resume mode contradicts it
	if f.sessionGiven {
		if f.resume || f.cont {
			return errors.New("--session cannot be combined with --resume or --continue")
		} else if _, err := session.ValidateName(f.sessionName); err != nil {
			return err
		}
	}
	if f.prompt == "" {
		if len(f.headless) > 0 {
			return fmt.Errorf("%s only applies with --prompt", strings.Join(f.headless, ", "))
		}
		return nil
	}
	switch {
	case f.allowAll && f.readOnly:
		return errors.New("--allow-all and --read-only are mutually exclusive")
	case f.resume && f.resumeID == "":
		return errors.New("--prompt needs --resume <id|name>; the bare session picker requires a terminal")
	case len(f.args) > 0:
		return errors.New("--prompt takes the whole prompt; remove the trailing arguments")
	case !slices.Contains([]string{outputText, outputJSON}, f.output):
		return fmt.Errorf("unknown --output %q, want text or json", f.output)
	}
	return nil
}

// scope returns the tool scope this command line asks for.
func (f cliFlags) scope() toolScope {
	switch {
	case f.allowAll:
		return scopeAllowAll
	case f.readOnly:
		return scopeReadOnly
	default:
		return scopeDefault
	}
}
