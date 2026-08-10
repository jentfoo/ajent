package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

func main() {
	render := flag.String("render", "auto",
		"paint mode: auto, inline (terminal scrollback, unsupported under tmux or screen), "+
			"alt (own scrollback), plain")
	flag.Parse()

	mode, ok := tui.ParseMode(*render)
	if !ok {
		fmt.Fprintf(os.Stderr, "ajent: unknown render mode %q\n", *render)
		os.Exit(2)
	}

	file, warnings, err := llm.LoadUserFile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		os.Exit(1)
	}
	reg, regWarnings := llm.NewRegistry(file, llm.LoadUserCache(), llm.RegistryOptions{})
	warnings = append(warnings, regWarnings...)

	// no configured model means no model to name, rather than a demo placeholder
	// that claims to be something the user never selected
	var model string
	maxTokens := demoMaxTokens
	if active := reg.Active(); active.ID != "" {
		model = active.Key()
		if active.ContextWindow > 0 {
			maxTokens = active.ContextWindow
		}
	}
	ui, err := tui.New(tui.Options{
		Mode:      mode,
		Model:     model,
		MaxTokens: maxTokens,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		os.Exit(1)
	}
	defer ui.Close()

	for _, w := range warnings {
		ui.Notify(w, tui.LevelWarn)
	}
	go refreshModels(ui, reg)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		ui.Close()
		os.Exit(0)
	}()

	d := newDemo(ui)
	for msg := range ui.Messages() {
		if cmd, arg, ok := slashCommand(msg); ok {
			handleCommand(ui, reg, cmd, arg)
			continue
		}
		ui.UserEcho(msg)
		d.play(msg)
	}
}
