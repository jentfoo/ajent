package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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

	ui, err := tui.New(tui.Options{
		Mode:      mode,
		Model:     demoModel,
		MaxTokens: demoMaxTokens,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		os.Exit(1)
	}
	defer ui.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		ui.Close()
		os.Exit(0)
	}()

	d := newDemo(ui)
	for msg := range ui.Messages() {
		ui.UserEcho(msg)
		d.play(msg)
	}
}
