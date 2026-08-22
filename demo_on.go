//go:build demo

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
)

// siblingServer names the standalone scripted server binary, expected next to
// this executable so both land in bin/ from one make build-demo.
const siblingServer = "ajent-demosrv"

// startDemo configures a hermetic demo run: it points AJENT_HOME at a fresh temp
// dir and spawns the sibling scripted model server, then returns a stop func that
// kills the child and removes the dir. Missing sibling is a hard error.
func startDemo() func() {
	home, err := os.MkdirTemp("", "ajent-demo-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: cannot make temp home: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv(config.EnvHome, home)

	srv := filepath.Join(filepath.Dir(mustExecutable()), siblingServer)
	if _, statErr := os.Stat(srv); statErr != nil {
		fmt.Fprintf(os.Stderr,
			"demo: %s not found; build it with `make build-demo`\n", srv)
		os.Exit(1)
	}

	cmd := exec.Command(srv, "-addr", "127.0.0.1:0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		os.Exit(1)
	}
	if err = cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "demo: cannot start %s: %v\n", srv, err)
		os.Exit(1)
	}

	url := readBaseURL(stdout)

	writeDemoConfig(home, url)

	return func() { stopDemo(cmd, home) }
}

// mustExecutable returns the running binary's path, exiting on failure.
func mustExecutable() string {
	p, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		os.Exit(1)
	}
	return p
}

// readBaseURL waits for the server's one-line base URL on stdout.
func readBaseURL(r io.ReadCloser) string {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: cannot read server url: %v\n", err)
		os.Exit(1)
	}
	return strings.TrimSpace(line)
}

// writeDemoConfig writes models.json and config.json into home so the run is
// self-contained: a demo provider pointing at the server plus find/grep/ls on.
func writeDemoConfig(home, url string) {
	models := fmt.Sprintf(`{
  "defaultModel": "demo/ajent-demo-1",
  "providers": {
    "demo": {
      "api": "openai-completions",
      "flavor": "generic",
      "baseUrl": %q,
      "models": [
        { "id": "ajent-demo-1", "contextWindow": 200000, "maxTokens": 8192,
          "compat": { "supportsParallelToolCalls": true } }
      ]
    }
  }
}`, url)
	if err := os.WriteFile(filepath.Join(home, llm.ModelsFileName), []byte(models), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		os.Exit(1)
	}

	const conf = `{
  "tools": { "enabled": ["read", "write", "edit", "bash", "find", "grep", "ls"] },
  "permissions": { "mode": "allow-read" },
  "ui": { "theme": "dark" }
}
`
	if err := os.WriteFile(filepath.Join(home, config.ConfigFileName), []byte(conf), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		os.Exit(1)
	}
}

// stopDemo kills the server child and removes the temp home exactly once.
func stopDemo(cmd *exec.Cmd, home string) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	_ = os.RemoveAll(home)
}
