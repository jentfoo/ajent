package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/pflag"
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
var headlessFlagNames = []string{"output", "allow-all", "read-only", "allow-tools", "deny-tools"}

// cliFlags is one parsed command line. The headless fields apply only when
// prompt is set.
type cliFlags struct {
	model    string
	render   string
	cont     bool
	resume   bool   // --resume was given, with or without an id
	resumeID string // the id from --resume <id>

	prompt     string
	output     string
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
	// --resume takes an optional id, which no flag package can express, so it is
	// lifted out of argv before parsing.
	f.resume, f.resumeID, argv = extractResume(argv)

	fs := pflag.NewFlagSet("ajent", pflag.ContinueOnError)
	fs.StringVarP(&f.model, "model", "m", "", "initial model to use")
	fs.StringVar(&f.render, "render", "auto",
		"paint mode: auto, inline (terminal scrollback, unsupported under tmux or screen), "+
			"alt (own scrollback), plain")
	fs.BoolVar(&f.cont, "continue", false, "resume the most recent session automatically")
	fs.StringVarP(&f.prompt, "prompt", "p", "",
		"run one turn non-interactively from this prompt, print the result and exit")
	fs.StringVarP(&f.output, "output", "o", outputText,
		"one-shot output shape: text (the final answer) or json (one event per line)")
	fs.BoolVar(&f.allowAll, "allow-all", false, "one-shot: offer every tool, bash included")
	fs.BoolVar(&f.readOnly, "read-only", false, "one-shot: offer only read-only tools")
	fs.StringSliceVar(&f.allowTools, "allow-tools", nil, "one-shot: extra tool names to offer")
	fs.StringSliceVar(&f.denyTools, "deny-tools", nil, "one-shot: tool names to withhold")

	fs.Usage = func() {
		out := os.Stderr
		_, _ = fmt.Fprintf(out, "usage of %s:\n", os.Args[0])
		_, _ = fmt.Fprint(out, fs.FlagUsages())
		_, _ = fmt.Fprintln(out, "      --resume [id]           "+
			"list saved sessions and resume one; with an id, resume that session directly")
	}

	if err := fs.Parse(argv); err != nil {
		return f, err
	}
	f.args = fs.Args()
	for _, name := range headlessFlagNames {
		if fs.Changed(name) {
			f.headless = append(f.headless, "--"+name)
		}
	}
	return f, nil
}

// validate reports the first usage error in the parsed command line.
func (f cliFlags) validate() error {
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
		return errors.New("--prompt needs --resume <id>; the bare session picker requires a terminal")
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
