package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/pflag"

	"github.com/jentfoo/ajent/pkg/app"
	"github.com/jentfoo/ajent/pkg/version"
)

func main() {
	f, err := parseFlags(os.Args[1:])
	if errors.Is(err, pflag.ErrHelp) {
		return // help already printed; exit cleanly
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
		os.Exit(app.ExitUsage)
	}
	// --version short-circuits before validation and every setup so it works with a
	// broken home dir, no network, and any flag combination.
	if f.version {
		printVersion(os.Stdout)
		return
	}
	// --update reinstalls from @latest in the foreground then exits; it never opens
	// the TUI or starts a session. /update is the in-session form.
	if f.update {
		res := version.SelfUpdate(context.Background())
		fmt.Println(res.Notice())
		if res.Err != nil {
			os.Exit(app.ExitUsage)
		}
		return
	}
	if verr := f.validate(); verr != nil {
		fmt.Fprintf(os.Stderr, "ajent: %v\n", verr)
		os.Exit(app.ExitUsage)
	}
	// --delete and --delete-old remove saved sessions then exit; they never open the
	// TUI or start one.
	if f.deleteGiven || f.deleteOld {
		os.Exit(app.RunDelete(os.Stdout, os.Stdin, app.DeleteOptions{
			Target:  f.deleteTarget,
			OldDays: f.deleteOldDays,
		}))
	}

	// --resume overrides --continue; neither means a brand-new session. --session
	// cannot reach here alongside either, validate() rejects that combination.
	sessMode, sessTarget := app.ResumeNewSession, ""
	if f.cont {
		sessMode = app.ResumeContinue
	}
	switch {
	case f.sessionName != "":
		sessMode, sessTarget = app.ResumeSessionName, f.sessionName
	case f.resume && f.resumeID != "":
		sessMode, sessTarget = app.ResumeID, f.resumeID // reopen that exact saved transcript
	case f.resume:
		sessMode = app.ResumePick // pick among saved roots, then resume its leaf
	}

	// A requested session must resolve before the TUI opens; otherwise fail fast
	// with a clear message instead of silently starting a fresh transcript.
	if sessMode == app.ResumeID || sessMode == app.ResumeSessionName {
		if err := app.CheckSessionTarget(sessMode, sessTarget); err != nil {
			fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
			os.Exit(app.ExitUsage)
		}
	}

	// the demo build reconfigures itself before config.Load reads AJENT_HOME.
	stop := startDemo()
	defer stop()

	code := app.Run(app.RunOptions{
		Model:      f.model,
		Render:     f.render,
		Prompt:     f.prompt,
		Output:     f.output,
		Stats:      f.stats,
		Scope:      f.scope(),
		AllowTools: f.allowTools,
		DenyTools:  f.denyTools,
		Args:       f.args,

		SessMode:   sessMode,
		SessTarget: sessTarget,
	})
	if code != app.ExitOK {
		stop() // os.Exit skips the deferred demo teardown
		os.Exit(code)
	}
}

// printVersion writes the build version line to w.
func printVersion(w io.Writer) {
	_, _ = fmt.Fprintln(w, "ajent version", version.Version)
}
