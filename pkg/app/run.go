package app

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/jentfoo/ajent/pkg/version"
)

// Run carries the parsed command line through config load, model resolution and
// either a headless turn or the interactive driver. It returns an exit code.
func Run(o RunOptions) int {
	// the flag layer outranks every file layer, -m/--render stop being ad hoc
	flagLayer := config.Layer{Name: "flag"}
	if o.Model != "" {
		var err error
		flagLayer.Data, err = config.SetKey(flagLayer.Data, "model", o.Model)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "ajent:", err)
			return ExitUsage
		}
	}
	if o.Render != "" && o.Render != "auto" {
		flagLayer.Data, _ = config.SetKey(flagLayer.Data, "ui.render", o.Render)
	}
	// --system outranks any agent.systemPrompt set in a file layer
	if o.System != "" {
		flagLayer.Data, _ = config.SetKey(flagLayer.Data, "agent.systemPrompt", o.System)
	}
	set, warnings, err := config.Load(config.Options{
		Workspace: config.Cwd(),
		Flags:     flagLayer,
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "ajent:", err)
		return ExitUsage
	}

	modeName := set.Settings().UI.Render
	mode, ok := tui.ParseMode(modeName)
	if !ok || modeName == "" {
		_, _ = fmt.Fprintf(os.Stderr, "ajent: unknown render mode %q\n", modeName)
		return ExitUsage
	}
	colorName := set.Settings().UI.Color
	color, ok := tui.ParseColorProfile(colorName)
	if !ok {
		warnings = append(warnings, fmt.Sprintf("unknown ui.color %q, detecting instead", colorName))
	}
	themeName := set.Settings().UI.Theme
	pal, known := tui.LookupPalette(themeName)
	if !known {
		pal = tui.DefaultPalette()
		warnings = append(warnings, fmt.Sprintf("unknown ui.theme %q, using %q", themeName, pal.Name))
	}
	imagesName := set.Settings().UI.Images
	if _, _, known := tui.ResolveImageProtocol(imagesName); !known {
		warnings = append(warnings, fmt.Sprintf("unknown ui.images %q, detecting instead", imagesName))
		imagesName = ""
	}

	// attach mode mirrors a running session over its control socket: it opens no
	// local transcript and needs no models file, so it branches before either loads.
	// The remote run decides model, tools and permission mode; only the paint
	// settings above are this process's own. A bare --attach resolves to the
	// live-session picker inside RunAttach.
	if o.AttachGiven {
		return RunAttach(AttachOptions{
			Socket: o.Attach, Mode: mode, Color: color,
			Palette: pal, Images: imagesName, Warnings: warnings,
		})
	}

	tools.SetImagesEnabled(!set.Settings().Images.Block)
	tools.ApplyLimits(tools.LimitsFrom(set.Settings().Tools.Limits))

	// the registry is built once and shared by model resolution, a headless run
	// and the interactive driver, so every path reads one models file.
	reg, wregs, rerr := registryFor(set)
	if rerr != nil {
		_, _ = fmt.Fprintln(os.Stderr, "ajent:", rerr)
		return ExitUsage
	}
	warnings = append(warnings, wregs...)

	// -m already activated during the build: the flag layer folds into DefaultModel,
	// so Active() names it, or the first-model fallback when the name does not resolve.
	active := reg.Active()

	// serve mode replaces the terminal entirely: the control socket is the only
	// front end, so it is decided before any stream classification. It implies
	// control whatever control.enabled says: a serve run without a socket has
	// no prompt source at all, which is useless rather than safer.
	if o.Serve {
		return RunServe(ServeOptions{
			Set: set, Reg: reg, Active: active,
			SessMode:     o.SessMode,
			SessTarget:   o.SessTarget,
			SessResolved: o.SessResolved,
			Warnings:     warnings,
			Scope:        o.Scope,
		})
	}

	// a one-shot run never opens a terminal; neither does a piped invocation.
	// Both settle every permission ask without a dialog, so they share the
	// headless path rather than running the driver with a prompter that cannot
	// open. tmux and alt-screen still present TTYs, so this only fires on a real
	// pipe or redirect.
	route, text := routeStartup(o.Prompt, o.Args, os.Stdin, os.Stdout)
	if route == routeUsage {
		_, _ = fmt.Fprintln(os.Stderr, "ajent:", text)
		return ExitUsage
	}
	if route == routeHeadless {
		return RunHeadless(HeadlessOptions{
			Set: set, Reg: reg, Active: active,
			SessMode:     o.SessMode,
			SessTarget:   o.SessTarget,
			SessResolved: o.SessResolved,
			Warnings:     warnings,
			Prompt:       text,
			Output:       o.Output,
			Stats:        o.Stats,
			Scope:        o.Scope,
			AllowTools:   o.AllowTools,
			DenyTools:    o.DenyTools,
		})
	}

	var label, short string
	if active.ID != "" {
		label = active.Key()
		short = active.ShortName()
	}
	ui, err := tui.New(tui.Options{
		Mode:       mode,
		Color:      color,
		Palette:    pal,
		Images:     imagesName,
		Model:      label,
		ModelShort: short,
		MaxTokens:  active.ContextWindow,
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "ajent:", err)
		return ExitUsage
	}
	defer ui.Close()

	for _, w := range warnings {
		ui.Notify(w, tui.LevelWarn)
	}

	go func() {
		if len(reg.ProviderNames()) == 0 {
			return // nothing configured yet, the setup wizard owns the first provider
		}
		notify := func(msg string, warn bool) { ui.Notify(msg, levelOf(warn)) }
		if added := reg.RefreshModels(notify); added > 0 {
			ui.NotifyKeyed("models", "discovered "+strconv.Itoa(added)+" more models", tui.LevelInfo)
		}
	}()
	if !set.Settings().DisableUpdateCheck {
		// user opted out of the startup nag entirely when disabled
		go version.CheckForUpdate(func(msg string) { ui.Notify(msg, tui.LevelInfo) })
	}

	resumeLabel, derr := Driver(ui, set, reg, active, o.SessMode, o.SessTarget, o.SessResolved, o.Args, o.Scope)
	if derr != nil {
		_, _ = fmt.Fprintln(os.Stderr, "ajent:", derr)
		return ExitUsage
	}

	// tell the user how to get back to this conversation
	ui.Close()
	if resumeLabel != "" {
		fmt.Print(resumeHint(resumeLabel))
	}
	return ExitOK
}

// RunServe drives the session with the control socket as its only front end
// and returns the process exit code. No terminal exists: prompts arrive over
// the socket, dialogs hold for a controller, and the run ends on signal. The
// resume hint lands on stderr, where a supervisor can read it.
func RunServe(o ServeOptions) int {
	for _, w := range o.Warnings {
		_, _ = fmt.Fprintln(os.Stderr, w)
	}
	label, err := Driver(nil, o.Set, o.Reg, o.Active, o.SessMode, o.SessTarget, o.SessResolved, nil, o.Scope)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "ajent:", err)
		return ExitUsage
	}
	if label != "" {
		_, _ = fmt.Fprint(os.Stderr, resumeHint(label))
	}
	return ExitOK
}

// startupRoute names the driver an invocation's streams classify onto.
type startupRoute uint8

const (
	routeInteractive startupRoute = iota // terminal stdin and stdout: open the TUI driver
	routeHeadless                        // a prompt is settled: run one turn without a front end
	routeUsage                           // nothing can run; usage text explains why
)

// argvPrompt joins positional arguments into the bootstrap prompt they carry,
// empty when nothing usable was given.
func argvPrompt(args []string) string {
	return strings.TrimSpace(strings.Join(args, " "))
}

// routeStartup classifies an invocation by its prompt sources (the -p flag,
// positional arguments, piped stdin) and process streams. It returns which
// driver should run plus either the prompt to answer for routeHeadless or the
// usage message to print for routeUsage.
func routeStartup(prompt string, args []string, stdin, stdout *os.File) (startupRoute, string) {
	if p := strings.TrimSpace(prompt); p != "" {
		// the flag outranks every other source, piped bytes beside -p are never read
		return routeHeadless, p
	}
	if isTerminal(stdin) && isTerminal(stdout) {
		// a terminal pair opens the TUI, where positional arguments seed the first prompt
		return routeInteractive, ""
	}
	if p := argvPrompt(args); p != "" {
		// positional arguments carry as explicit a prompt as the flag: with no editor
		// to open it runs headless, and stdin beside it is never drained either
		return routeHeadless, p
	}
	if isTerminal(stdin) {
		// a typed-in invocation with redirected stdout has nothing to run and
		// nowhere interactive to go
		return routeUsage, "stdout is not a terminal; use --prompt or pipe a prompt on stdin"
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return routeUsage, "reading piped stdin: " + err.Error()
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		// an emptied pipe leaves stdin at EOF: the TUI cannot take over and no
		// prompt exists to run, so this is the usage error a redirect already gives
		return routeUsage, "piped stdin was empty; use --prompt or pipe a prompt on stdin"
	}
	return routeHeadless, text
}

// isTerminal reports whether f is a character device, the same test the TUI
// applies to decide whether a full-screen render is possible.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// registryFor loads the user models file and builds a Registry over it. It
// returns load warnings plus an error when the models file cannot be read.
func registryFor(set *config.Set) (*llm.Registry, []string, error) {
	file, w, err := llm.LoadUserFile()
	if err != nil {
		return nil, nil, err
	}
	warnings := slices.Clone(w)
	if m := set.Settings().Model; m != "" {
		file.DefaultModel = m
	}
	reg, rw := llm.NewRegistry(file, llm.LoadUserCache(), llm.RegistryOptions{})
	warnings = append(warnings, rw...)
	// models declaring no compactThreshold of their own follow the config setting,
	// so the trigger, the context bar and the compaction tail all read one number.
	reg.SetCompactDefault(set.Settings().Compaction.Threshold)
	return reg, warnings, nil
}
