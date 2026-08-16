// Command ajent-demosrv is a standalone scripted OpenAI-compatible chat service:
// it speaks the chat-completions SSE dialect and plays a fixed sequence of real
// tool calls, so any agent harness can run its whole loop against it.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jentfoo/ajent/demo/srv"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address; :0 picks a free port")
	flag.Parse()

	server, err := srv.Start(*addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent-demosrv:", err)
		os.Exit(1)
	}
	defer func() { _ = server.Close() }()
	fmt.Println(server.URL())
	_ = os.Stdout.Sync() // the parent reads this line to learn the port

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}
