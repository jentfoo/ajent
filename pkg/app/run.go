package app

import (
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"syscall"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/jentfoo/ajent/pkg/version"
)

// Run carries the parsed command line through config load, model resolution and
// either a headless turn or the interactive driver. It returns an exit code.
func Run(o RunOptions) int {
	// the flag layer outranks every file layer; -m/--render stop being ad hoc.
	flagLayer := config.Layer{Name: "flag"}
	if o.Model != "" {
		var err error
		flagLayer.Data, err = config.SetKey(flagLayer.Data, "model", o.Model)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ajent:", err)
			return ExitUsage
		}
	}
	if o.Render != "" && o.Render != "auto" {
		flagLayer.Data, _ = config.SetKey(flagLayer.Data, "ui.render", o.Render)
	}
	set, warnings, err := config.Load(config.Options{
		Workspace: config.Cwd(),
		Flags:     flagLayer,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		return ExitUsage
	}

	modeName := set.Settings().UI.Render
	mode, ok := tui.ParseMode(modeName)
	if !ok || modeName == "" {
		fmt.Fprintf(os.Stderr, "ajent: unknown render mode %q\n", modeName)
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
	tools.ApplyLimits(tools.LimitsFrom(set.Settings().Tools.Limits))

	// the registry is built once and shared by model resolution, a headless run
	// and the interactive driver, so every path reads one models file.
	reg, wregs, rerr := registryFor(set)
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "ajent:", rerr)
		return ExitUsage
	}
	warnings = append(warnings, wregs...)

	active := resolveActiveModel(o.Model, reg)

	// a one-shot run never opens a terminal.
	if o.Prompt != "" {
		return RunHeadless(HeadlessOptions{
			Set: set, Reg: reg, Active: active,
			SessMode:   o.SessMode,
			SessTarget: o.SessTarget,
			Warnings:   warnings,
			Prompt:     o.Prompt,
			Output:     o.Output,
			Stats:      o.Stats,
			Scope:      o.Scope,
			AllowTools: o.AllowTools,
			DenyTools:  o.DenyTools,
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
		Model:      label,
		ModelShort: short,
		MaxTokens:  active.ContextWindow,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		return ExitUsage
	}
	defer ui.Close()

	for _, w := range warnings {
		ui.Notify(w, tui.LevelWarn)
	}

	// a terminating signal closes the UI, whose closed message channel ends the
	// driver loop, so teardown and the resume hint run instead of a hard exit
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		ui.Close()
	}()

	go func() {
		notify := func(msg string, warn bool) { ui.Notify(msg, levelOf(warn)) }
		if added := reg.RefreshModels(notify); added > 0 {
			ui.NotifyKeyed("models", "discovered "+strconv.Itoa(added)+" more models", tui.LevelInfo)
		}
	}()
	if !set.Settings().DisableUpdateCheck {
		// user opted out of the startup nag entirely when disabled
		go version.CheckForUpdate(func(msg string) { ui.Notify(msg, tui.LevelInfo) })
	}

	resumeLabel := Driver(ui, set, reg, active, o.SessMode, o.SessTarget, o.Args)

	// Restore the terminal before printing so the hint is visible after a Ctrl+C /
	// Ctrl+D quit, then tell the user how to get back to this conversation.
	ui.Close()
	if resumeLabel != "" {
		fmt.Printf("\nRun `ajent --resume %s` to resume this session.\n", resumeLabel)
	}
	return ExitOK
}

func resolveActiveModel(modelFlag string, reg *llm.Registry) llm.Model {
	active := reg.Active()
	if modelFlag != "" {
		if m, rerr := reg.Resolve(modelFlag); rerr == nil {
			reg.SetActive(m)
			return m
		}
	}
	return active
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
