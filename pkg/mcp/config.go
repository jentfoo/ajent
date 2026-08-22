// Package mcp is the Model Context Protocol client: it connects to stdio and
// network MCP servers declared in mcp.json, bridges their tools into agent.Tool,
// and supervises each server's lifecycle. The protocol layer delegates to
// github.com/mark3labs/mcp-go; what we own here is config, namespacing, enable
// state, progress mapping and process supervision.
package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/config"
)

// ServerConfig is one server declaration from mcp.json. Exactly one of Command
// and URL selects the transport; the rest tune lifecycle and filtering.
type ServerConfig struct {
	Command      string            `json:"command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Transport    string            `json:"transport,omitempty"` // "http", "stdio" or legacy "sse"; inferred when absent
	URL          string            `json:"url,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Enabled      *bool             `json:"enabled,omitempty"`
	Tools        ToolFilter        `json:"tools,omitzero"`
	ExcludeTools []string          `json:"excludeTools,omitempty"` // exact tool names omitted from registration
	ReadOnly     FlexStrings       `json:"readOnly,omitempty"`     // globs, or true to mark every tool read-only
	Timeout      FlexDuration      `json:"timeout,omitempty"`      // ms number or Go duration string
}

// mcpDoc is the declared shape of an mcp.json file for UnknownKeys checking.
type mcpDoc struct {
	Servers map[string]ServerConfig `json:"servers"`
}

// ToolFilter is the per-server allow/deny glob set applied before registration.
type ToolFilter struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// Transport kinds a server may declare or infer. stdio runs a command; http and
// sse dial a url.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"
)

// LoadConfig reads ~/.ajent/mcp.json then <workspace>/.ajent/mcp.json and merges
// them by server name with whole-entry replacement (project wins). Warnings for
// anything questionable in either file are returned alongside the merged map.
func LoadConfig(workspace string) (map[string]ServerConfig, []string, error) {
	merged := make(map[string]ServerConfig)
	var warnings []string

	for _, path := range mcpFiles(workspace) {
		servers, warns, err := loadOne(path)
		if err != nil {
			return nil, warnings, err
		}
		warnings = append(warnings, warns...)
		for name, cfg := range servers { // whole-entry replacement: project wins
			merged[name] = cfg
		}
	}

	var validated []string
	for name, cfg := range merged {
		if err := validateServer(name, cfg); err != nil {
			return nil, warnings, err
		}
		validated = append(validated, name)
	}
	slices.Sort(validated)
	out := make(map[string]ServerConfig, len(merged))
	for _, n := range validated { // deterministic iteration order
		out[n] = merged[n]
	}
	return out, warnings, nil
}

// mcpFiles returns the user then project config paths, in merge order.
func mcpFiles(workspace string) []string {
	var paths []string
	if dir, err := config.Dir(); err == nil {
		paths = append(paths, filepath.Join(dir, "mcp.json"))
	}
	return append(paths, filepath.Join(config.ProjectDir(workspace), "mcp.json"))
}

// loadOne decodes a single mcp.json file following pkg/llm's LoadFile read path.
func loadOne(path string) (map[string]ServerConfig, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	} else if os.IsNotExist(err) || len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil, nil // no file (or empty) is not an error
	}
	data := config.RelaxJSON(raw)

	var doc mcpDoc
	if err = json.Unmarshal(data, &doc); err != nil {
		return nil, nil, config.JSONError(path, raw, err)
	}

	warnings := make([]string, 0, 4)
	unknown, _ := config.UnknownKeys(data, doc) // json.Unmarshal already validated the same data
	for _, k := range unknown {
		warnings = append(warnings, fmt.Sprintf("%s: unrecognized key %q", path, k))
	}
	for _, k := range config.DuplicateKeys(data) {
		warnings = append(warnings, fmt.Sprintf("%s: duplicate key %q, the last one wins", path, k))
	}

	var expandedErr error
	out := make(map[string]ServerConfig, len(doc.Servers))
	for name, cfg := range doc.Servers {
		expanded, err := expandEnv(cfg)
		if err != nil && expandedErr == nil {
			expandedErr = fmt.Errorf("%s: server %q: %w", path, name, err)
		}
		out[name] = expanded
	}
	return out, warnings, expandedErr
}

// expandEnv expands ${env:VAR} references in Env values, Headers and URL. An
// unset variable is an error naming it, never an empty value.
func expandEnv(cfg ServerConfig) (ServerConfig, error) {
	for k, v := range cfg.Env {
		x, err := expandVar(v)
		if err != nil {
			return cfg, fmt.Errorf("env %q: %w", k, err)
		}
		cfg.Env[k] = x
	}
	for k, v := range cfg.Headers {
		x, err := expandVar(v)
		if err != nil {
			return cfg, fmt.Errorf("headers %q: %w", k, err)
		}
		cfg.Headers[k] = x
	}
	if cfg.URL != "" {
		x, err := expandVar(cfg.URL)
		if err != nil {
			return cfg, fmt.Errorf("url: %w", err)
		}
		cfg.URL = x
	}
	return cfg, nil
}

// expandVar substitutes a single ${env:VAR} reference in s.
func expandVar(s string) (string, error) {
	if !strings.Contains(s, "${env:") {
		return s, nil
	}
	var b strings.Builder
	for {
		i := strings.Index(s, "${env:")
		if i < 0 {
			b.WriteString(s)
			break
		}
		end := strings.IndexByte(s[i+len("${env:"):], '}')
		if end < 0 {
			return "", fmt.Errorf("unterminated ${env:...} in %q", s)
		}
		name := s[i+len("${env:") : i+len("${env:")+end]
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return "", fmt.Errorf("environment variable %s is unset; set it before starting ajent", name)
		}
		b.WriteString(s[:i])
		b.WriteString(v)
		s = s[i+len("${env:")+end+1:]
	}
	return b.String(), nil
}

// NetworkKind returns the resolved transport kind for a network (url) server:
// "http" or legacy "sse". Only meaningful when Command is empty.
func (c ServerConfig) NetworkKind() string {
	if c.Transport == TransportSSE {
		return TransportSSE
	}
	return TransportHTTP
}

// validateServer checks a single server's declaration: exactly one of command or
// url, a known transport consistent with it, and an unknown startup name is an
// error that names the offending value.
func validateServer(name string, cfg ServerConfig) error {
	switch {
	case cfg.Command != "" && cfg.URL != "":
		return fmt.Errorf("server %q: declare either command or url, not both", name)
	case cfg.Command == "" && cfg.URL == "":
		return fmt.Errorf("server %q: need a command (stdio) or url (network)", name)
	}
	t := transportKind(cfg)
	switch t { // stdio needs a command; http/sse need a url
	case TransportStdio:
		if cfg.Command == "" {
			return fmt.Errorf("server %q: transport %q requires a command", name, t)
		}
	default:
		if cfg.URL == "" {
			return fmt.Errorf("server %q: transport %q requires a url", name, t)
		}
	}
	return nil
}
