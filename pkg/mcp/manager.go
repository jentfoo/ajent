package mcp

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/jentfoo/ajent/pkg/agent"
)

// State is a server tool's visibility, mirrored from the registry's enum so
// pkg/mcp does not import pkg/tools.
type State uint8

const (
	StateDisabled State = iota // known, not in the prompt, not callable
	StateEnabled               // in the prompt and callable
)

// Registrar is what a server registers its bridged tools into. main.go passes
// the tool registry; declaring it here keeps pkg/mcp free of pkg/tools.
type Registrar interface {
	RegisterState(source string, t agent.Tool, s State)
	Unregister(source string)
	EnabledNames(source string) []string  // currently enabled names under source
	DisabledNames(source string) []string // currently disabled (inactive) names under source
	AllNames(source string) []string      // every registered name under source, any state
	MarkReadOnly(names []string)          // record read-only publication metadata by name
}

// Options configures the manager with adapters from the front end.
type Options struct {
	Registrar Registrar
	Notice    func(msg string, warn bool)
	Status    func(text string) // status segment text, "" clears it
	Restore   []string          // persisted tools.enabled names to honour on connect
	Workspace string            // for re-reading mcp.json on /mcp reload
}

// server is one configured server and its live client (nil when disconnected).
type server struct {
	name          string
	cfg           ServerConfig
	source        string  // "mcp: <name>", the /tools grouping key
	c             *Client // nil while disconnected
	logs          *ringLog
	defs          []ToolDef           // last filtered tool list, for status/tool groups and drift compare
	resources     []Resource          // discovered on connect, exposed to callers
	prompts       []PromptDef         // discovered on connect, no UI yet
	failures      int                 // consecutive connect failures, for backoff and notices
	down          bool                // a reconnect loop is active; suppresses the already-connected check
	reopenKeep    map[string]struct{} // enabled set captured at death, restored on reconnect
	rediscovering bool                // a list_changed re-discovery is in flight; coalesces bursts

	notice func(string, bool) // notice sink over the manager's Options; immutable, so no lock

	mu sync.Mutex // sole guard for every mutable field above; m.mu covers only the servers map
}

// Manager supervises every configured MCP server's lifecycle.
type Manager struct {
	opts Options

	ctx    context.Context // long-lived for reconnect loops; canceled on Close
	cancel context.CancelFunc

	mu      sync.Mutex // guards servers and loaded only; per-server state lives under server.mu
	servers map[string]*server
	loaded  bool // first-message load has run (LoadOnFirstMessage)
}

// New returns a manager over the given servers. Restore names are applied to each
// registered tool so a resumed session keeps its enabled set even though MCP
// registers after the resume replay.
func New(cfg map[string]ServerConfig, opts Options) *Manager {
	m := &Manager{opts: opts, servers: make(map[string]*server)}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	for name, sc := range cfg {
		m.servers[name] = m.newServer(name, sc)
	}
	return m
}

func (m *Manager) newServer(name string, cfg ServerConfig) *server {
	return &server{
		name:   name,
		cfg:    cfg,
		source: "mcp: " + name,
		logs:   newRingLog(200),
		notice: func(msg string, warn bool) {
			if m.opts.Notice != nil {
				m.opts.Notice("mcp "+name+": "+msg, warn)
			}
		},
	}
}

// Source returns the grouping label for a server name.
func (m *Manager) Source(name string) string {
	if s := m.serverByName(name); s != nil {
		return s.source
	}
	return "mcp: " + name
}

func (m *Manager) serverByName(name string) *server {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.servers[name]
}

// LoadOnFirstMessage connects and registers every server, exactly once,
// just before the user's first message is assembled into a turn. Doing it here,
// not at session start, means any /tools or /mcp changes made up to that point
// take effect; later prompts never re-trigger it. Config-disabled servers still
// register their tools as StateDisabled so they remain visible and toggleable in
// /tools.
func (m *Manager) LoadOnFirstMessage(ctx context.Context) {
	m.mu.Lock()
	if m.loaded {
		m.mu.Unlock()
		return
	}
	m.loaded = true
	names := make([]string, 0, len(m.servers))
	for n := range m.servers {
		names = append(names, n)
	}
	slices.Sort(names)
	m.mu.Unlock()

	for _, name := range names { // every server connects eagerly on the first message
		s := m.serverByName(name)
		if s != nil {
			_ = m.connect(ctx, name) // errors surface via notice/status
		}
	}
	m.updateStatus() // publish the initial active/discovered ratio
}

// Connect dials and registers a server's tools. Idempotent for an already
// connected server.
func (m *Manager) Connect(ctx context.Context, name string) error {
	return m.connect(ctx, name)
}

func (m *Manager) connect(ctx context.Context, name string) error {
	s := m.serverByName(name)
	if s == nil {
		return fmt.Errorf("no MCP server %q", name)
	}
	s.mu.Lock()
	already := !s.down && s.c != nil && s.failures == 0
	s.mu.Unlock()
	if already {
		return nil
	}
	keepEnabled := toSet(m.opts.Registrar.EnabledNames(s.source)) // registrar call stays off s.mu
	s.mu.Lock()
	for k := range s.reopenKeep { // restore the pre-death enabled set across a reconnect
		keepEnabled[k] = struct{}{}
	}
	s.mu.Unlock()

	c, err := Connect(ctx, name, s.config())
	if err != nil {
		// an unreachable server is expected (offline or not yet started); keep the
		// reason out of notices — visible in /mcp logs and the status ratio only.
		s.diag("connect failed: " + err.Error())
		m.updateStatus() // this server contributes nothing to the ratio until it connects
		return err
	}
	c.SetNotice(func(msg string) { s.note(strings.TrimPrefix(msg, "mcp "+name+": "), true) })

	// discovery is bounded so an unresponsive server surfaces as a connect error
	// instead of hanging LoadOnFirstMessage / reload (mirrors rediscan).
	dctx, dcancel := context.WithTimeout(ctx, discoverTimeout)
	defer dcancel()
	defs, err := c.Tools(dctx)
	if err != nil {
		s.note(err.Error(), true)
		_ = c.Close()
		return fmt.Errorf("mcp %s: discover: %w", name, err)
	}
	resources, rerr := c.Resources(dctx) // best effort; tools drive registration
	if rerr != nil {
		s.diag("resources/list failed: " + rerr.Error())
	}
	prompts, perr := c.Prompts(dctx)
	if perr != nil {
		s.diag("prompts/list failed: " + perr.Error())
	}
	// drop anything registered under this source before bridging the fresh list
	m.opts.Registrar.Unregister(s.source)
	s.mu.Lock()
	s.resources = resources
	s.prompts = prompts
	s.c = c
	s.down = false // a reconnect loop succeeded; clear its state so future connects short-circuit again
	s.reopenKeep = nil
	s.failures = 0
	s.mu.Unlock()
	m.register(s, c, defs, keepEnabled) // register never fails; it logs and continues
	go m.watchServer(s)
	// m.ctx, not the caller's: notification refresh outlives whoever connected
	c.OnNotification(func(n mcp.JSONRPCNotification) { m.onNotification(m.ctx, s, n) })
	s.diag("connected (" + c.Transport() + ")")
	return nil
}

// register bridges live defs into the registry under s.source. forceEnabled names
// stay enabled regardless of restore, preserving live state across a re-register.
func (m *Manager) register(s *server, c *Client, defs []ToolDef, forceEnabled map[string]struct{}) {
	cfg := s.config()
	defs = filterTools(defs, cfg.Tools, cfg.ExcludeTools)
	dur := time.Duration(cfg.Timeout)

	var restore map[string]struct{}
	if len(m.opts.Restore) > 0 { // a resumed session's enabled set is authoritative
		restore = toSet(m.opts.Restore)
	}
	disabledByCfg := disabledByConfig(cfg) // config-disabled stays inactive by default

	for _, d := range defs {
		n := s.name + "__" + d.Name
		tool := Bridge(s.name, d, c, BridgeOptions{ReadOnly: d.ReadOnly, Timeout: dur})
		st := StateDisabled // known but inactive by default; config-off or a restored subset leaves the rest off
		if !disabledByCfg && restore == nil {
			st = StateEnabled // fully-enabled, fresh server exposes everything
		} else if has(restore, n) {
			// an explicit enabled set is authoritative: /tools enablements win over the
			// config default, and a restored subset keeps only its members on
			st = StateEnabled
		}
		if forceEnabled != nil && has(forceEnabled, n) { // live registry state wins over restore/default
			st = StateEnabled
		}
		m.opts.Registrar.RegisterState(s.source, tool, st)
	}

	if ro := readOnlyOf(defs, s.name); len(ro) > 0 { // sub-agent publication metadata
		m.opts.Registrar.MarkReadOnly(ro)
	}

	s.mu.Lock()
	s.defs = defs
	s.mu.Unlock()
	s.diag(fmt.Sprintf("%d tools registered", len(defs)))
	m.updateStatus() // the active/discovered ratio changed with registration
}

// readOnlyOf returns namespaced server__tool names marked read-only among defs,
// for recording publication metadata on the registry after registration.
func readOnlyOf(defs []ToolDef, server string) []string {
	var out []string
	for _, d := range defs {
		if d.ReadOnly {
			out = append(out, server+"__"+d.Name)
		}
	}
	slices.Sort(out)
	return out
}

// defsSnapshot returns a copy of the server's last filtered tool list.
func (s *server) defsSnapshot() []ToolDef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.defs)
}

// resourcesSnapshot returns a copy of the server's discovered resources.
func (s *server) resourcesSnapshot() []Resource {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.resources)
}

// promptsSnapshot returns a copy of the server's discovered prompt templates.
func (s *server) promptsSnapshot() []PromptDef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.prompts)
}

func has(set map[string]struct{}, k string) bool {
	_, ok := set[k]
	return ok
}

// Disconnect closes and unregisters a server without removing its config.
func (m *Manager) Disconnect(name string) {
	if s := m.serverByName(name); s != nil {
		m.disconnect(s)
	}
}

// disconnect closes a server by pointer, so a config reload can still close one it
// has already removed from the map.
func (m *Manager) disconnect(s *server) {
	s.mu.Lock()
	c := s.c
	s.c = nil
	s.down = false // stop any in-flight reconnect loop for this server
	s.reopenKeep = nil
	s.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	m.opts.Registrar.Unregister(s.source)
	m.updateStatus() // a disconnected server's tools leave the ratio until reconnected
}

// Reload re-reads mcp.json and reconciles: removed servers disconnect, new ones
// connect eagerly, and an existing server's config is replaced. See applyConfig
// for how much of a changed config a connected server can take on the spot.
func (m *Manager) Reload(ctx context.Context) error {
	servers, warns, err := LoadConfig(m.opts.Workspace)
	if err != nil {
		return err
	}
	for _, w := range warns {
		if m.opts.Notice != nil {
			m.opts.Notice("mcp: "+w, true)
		}
	}
	cfg := servers // whole map replaces the previous view by name

	var dropped []*server
	existing := make(map[string]*server, len(cfg))
	m.mu.Lock()
	for name, s := range m.servers {
		if _, ok := cfg[name]; !ok { // no longer configured
			dropped = append(dropped, s)
			delete(m.servers, name)
		}
	}
	for name, sc := range cfg {
		if s, ok := m.servers[name]; ok {
			existing[name] = s
		} else {
			m.servers[name] = m.newServer(name, sc)
		}
	}
	m.mu.Unlock()

	for _, s := range dropped { // by pointer: it is out of the map already
		m.disconnect(s)
	}
	for name, s := range existing {
		m.applyConfig(s, cfg[name])
	}

	for _, name := range m.ServerNames() { // every server connects eagerly on reload too
		s := m.serverByName(name)
		if s != nil && s.client() == nil {
			_ = m.connect(ctx, name) // errors surface via notice/status
		}
	}
	m.updateStatus() // reconcile changed which servers and tools are live
	return nil
}

// applyConfig replaces a server's config. A disconnected server picks it up whole
// on its next connect; a connected one re-registers any tool-filter change against
// the running process, and is left running with a notice when the transport itself
// changed, since restarting it would abort calls in flight.
func (m *Manager) applyConfig(s *server, sc ServerConfig) {
	s.mu.Lock()
	old, c := s.cfg, s.c
	s.cfg = sc
	s.mu.Unlock()
	if c == nil { // the next connect picks the new config up whole
		return
	}
	if filterChanged(old, sc) { // independent of the transport: applies against the running process
		m.rediscan(s)
	}
	if connectionChanged(old, sc) {
		s.note("connection config changed; applies on the next connect, or now with /mcp disconnect then /mcp connect", true)
	}
}

// connectionChanged reports whether a config edit can only take effect on a new
// transport.
func connectionChanged(a, b ServerConfig) bool {
	return a.Command != b.Command || a.URL != b.URL || a.Transport != b.Transport ||
		!slices.Equal(a.Args, b.Args) || !maps.Equal(a.Env, b.Env) || !maps.Equal(a.Headers, b.Headers)
}

// filterChanged reports whether a config edit changes which tools register or how
// they are called, so the live registration is stale.
func filterChanged(a, b ServerConfig) bool {
	return a.Timeout != b.Timeout || disabledByConfig(a) != disabledByConfig(b) ||
		!slices.Equal(a.Tools.Allow, b.Tools.Allow) || !slices.Equal(a.Tools.Deny, b.Tools.Deny) ||
		!slices.Equal(a.ExcludeTools, b.ExcludeTools) || !slices.Equal(a.ReadOnly, b.ReadOnly)
}

// disabledByConfig reports whether a config explicitly turns its server's tools off.
func disabledByConfig(cfg ServerConfig) bool { return cfg.Enabled != nil && !*cfg.Enabled }

// ServerNames returns configured server names in sorted order.
func (m *Manager) ServerNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.servers))
	for n := range m.servers {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Status returns a per-server summary row for /mcp.
func (m *Manager) Status(ctx context.Context) []ServerStatus {
	names := m.ServerNames()
	out := make([]ServerStatus, 0, len(names))
	for _, name := range names {
		out = append(out, m.serverStatus(ctx, name))
	}
	return out
}

func (m *Manager) serverStatus(ctx context.Context, name string) ServerStatus {
	s := m.serverByName(name)
	if s == nil {
		return ServerStatus{Name: name}
	}
	st := ServerStatus{
		Name:      name,
		Transport: transportKind(s.config()),
	}
	s.mu.Lock()
	c := s.c
	failures := s.failures
	down := s.down
	s.mu.Unlock()
	st.ToolCount = len(s.defsSnapshot()) // last discovered (filtered) tool count
	if c == nil {
		if down { // a reconnect loop is running; report progress rather than disconnected
			st.State = fmt.Sprintf("reconnecting (%d)", failures)
		} else {
			st.State = "disconnected"
		}
		return st
	}
	st.Connected = true
	pctx, pcancel := context.WithTimeout(ctx, pingTimeout)
	defer pcancel()
	start := time.Now()
	err := c.Ping(pctx)
	st.Latency = time.Since(start)
	if err != nil {
		st.State = "unresponsive"
	} else if failures > 0 {
		st.State = fmt.Sprintf("reconnecting (%d)", failures)
	} else {
		st.State = "connected"
	}
	return st
}

// ServerStatus is one /mcp row.
type ServerStatus struct {
	Name      string
	Transport string
	Connected bool
	State     string
	ToolCount int
	Latency   time.Duration
}

// ToolGroup is one server's /tools group metadata: its grouping key (the source
// label) and the full header text rendered above its tools.
type ToolGroup struct {
	Source string // "mcp: <name>", the /tools grouping key
	Label  string // e.g. "mcp: playwright  (12 tools, connected)"
}

// ToolGroups returns per-server group metadata for /tools headers.
func (m *Manager) ToolGroups() []ToolGroup {
	var out []ToolGroup
	for _, name := range m.ServerNames() {
		s := m.serverByName(name)
		if s == nil {
			continue
		}
		defs := s.defsSnapshot()
		s.mu.Lock()
		connected, failures := s.c != nil, s.failures
		src := s.source
		s.mu.Unlock()
		out = append(out, ToolGroup{
			Source: src,
			Label:  toolGroupLabel(name, len(defs), connected, failures),
		})
	}
	return out
}

// toolGroupLabel renders a /tools header for one server.
func toolGroupLabel(name string, count int, connected bool, failures int) string {
	state := "disconnected"
	switch {
	case !connected:
		// already disconnected
	case failures > 0:
		state = fmt.Sprintf("reconnecting (%d)", failures)
	default:
		state = "connected"
	}
	return fmt.Sprintf("mcp: %s  (%d tools, %s)", name, count, state)
}

// Logs returns a server's recent stderr and protocol lines.
func (m *Manager) Logs(name string) []string {
	s := m.serverByName(name)
	if s == nil {
		return nil
	}
	return slices.Clone(s.logs.lines())
}

// ServerResources returns a connected server's discovered resources, or nil when
// unknown.
func (m *Manager) ServerResources(name string) []Resource {
	s := m.serverByName(name)
	if s == nil {
		return nil
	}
	return slices.Clone(s.resourcesSnapshot())
}

// ServerPrompts returns a connected server's discovered prompt templates, or nil
// when unknown.
func (m *Manager) ServerPrompts(name string) []PromptDef {
	s := m.serverByName(name)
	if s == nil {
		return nil
	}
	return slices.Clone(s.promptsSnapshot())
}

// Close stops reconnect loops and disconnects every connected server.
func (m *Manager) Close() {
	if m.cancel != nil { // stop in-flight reconnect backoff on shutdown
		m.cancel()
	}
	for _, name := range m.ServerNames() {
		m.Disconnect(name)
	}
}

// onNotification routes a server notification to discovery or progress output.
// The client dispatches notifications asynchronously (see Client.OnNotification), so
// this never runs on mcp-go's transport reader goroutine; it may still do blocking I/O
// safely. list_changed re-discovery is further serialized per server and given its own
// bounded context so bursts cannot race the registry nor a dead server hang forever.
func (m *Manager) onNotification(ctx context.Context, s *server, n mcp.JSONRPCNotification) {
	switch n.Method {
	case string(mcp.MethodNotificationToolsListChanged):
		s.diag("tools changed; re-discovering")
		m.rediscan(s)
	case string(mcp.MethodNotificationResourcesListChanged), string(mcp.MethodNotificationPromptsListChanged):
		m.refreshCapabilities(ctx, s) // re-discover resources and prompts
	case string(mcp.MethodNotificationProgress):
		s.writeProgress(n.Params.AdditionalFields)
	default:
		s.diag("notification " + n.Method)
	}
}

// rediscoveryTimeout bounds one tools/list_changed re-discovery so an unresponsive
// server surfaces an error instead of leaking a goroutine or hanging the session.
const rediscoveryTimeout = 45 * time.Second

// pingTimeout bounds the /mcp liveness probe so a wedged-but-alive server reports
// unresponsive instead of hanging the command.
const pingTimeout = 2 * time.Second

// discoverTimeout bounds one connect's capability discovery (tools/resources/
// prompts) so an unresponsive server surfaces as a connect error instead of
// hanging the first-message load or /mcp reload that awaits it.
const discoverTimeout = 45 * time.Second

// rediscan re-discovers a connected server's tools and re-registers them, after a
// tools/list_changed notification or a reloaded tool filter. It runs in its own
// goroutine and is serialized per server: a second call while one pass is running is
// coalesced, since the in-flight pass reads the current tool set anyway.
func (m *Manager) rediscan(s *server) {
	s.mu.Lock()
	if s.rediscovering { // a refresh already in flight; it sees the latest state
		s.mu.Unlock()
		return
	}
	s.rediscovering = true
	c := s.c
	s.mu.Unlock()
	keepEnabled := toSet(m.opts.Registrar.EnabledNames(s.source))
	if c == nil {
		s.mu.Lock()
		s.rediscovering = false
		s.mu.Unlock()
		return
	}

	go func() { // off the caller's goroutine so re-registration never blocks a notification
		defer func() {
			s.mu.Lock()
			s.rediscovering = false
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(m.ctx, rediscoveryTimeout)
		defer cancel()
		defs, err := c.Tools(ctx)
		if err != nil {
			s.note("re-discover failed: "+err.Error(), true)
			return
		}
		s.mu.Lock() // bail if the client was replaced or disconnected mid-list
		live := s.c == c && !s.down
		s.mu.Unlock()
		if !live {
			return
		}
		m.opts.Registrar.Unregister(s.source)
		m.register(s, c, defs, keepEnabled)
	}()
}

// refreshCapabilities re-discovers a server's resources and prompts after their
// list_changed notifications. Failures are noted but never fatal.
func (m *Manager) refreshCapabilities(ctx context.Context, s *server) {
	c := s.client()
	if c == nil {
		return
	}
	resources, rerr := c.Resources(ctx)
	prompts, perr := c.Prompts(ctx)
	s.mu.Lock()
	if rerr != nil {
		s.diag("resources refresh failed: " + rerr.Error())
	} else {
		s.resources = resources
	}
	if perr != nil {
		s.diag("prompts refresh failed: " + perr.Error())
	} else {
		s.prompts = prompts
	}
	s.mu.Unlock()
}

// client returns the server's live client, or nil when disconnected.
func (s *server) client() *Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c
}

// config returns the server's current declaration, which /mcp reload may replace.
func (s *server) config() ServerConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// writeProgress routes a progress notification's fields to the matching call.
func (s *server) writeProgress(fields map[string]any) {
	token := fields["progressToken"]
	var key int64
	switch v := token.(type) {
	case float64:
		key = int64(v)
	default:
		return
	}
	value, _ := fields["progress"].(float64)
	c := s.client()
	if c == nil {
		return
	}
	c.writeProgress(key, fmt.Sprintf("progress %.0f", value))
}

// watchServer streams a stdio child's stderr into the ring log and, when the
// process exits (stderr EOF), reconnects with capped backoff. Network servers have
// no child to supervise and return immediately.
func (m *Manager) watchServer(s *server) {
	c := s.client()
	if c == nil {
		return
	}
	r := c.stderr()
	if r == nil { // network server: no process to watch for death
		return
	}
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n') // one log entry per line; unbounded so nothing is dropped
		if len(line) > 0 {
			text := strings.TrimRight(strings.TrimSuffix(string(line), "\n"), "\r")
			if text != "" {
				s.diag("stderr: " + text)
			}
		}
		if err != nil { // EOF or read error means the child exited
			break
		}
	}
	m.reconnect(s)
}

// maxReconnectWait caps the exponential backoff between reconnection attempts.
const maxReconnectWait = 30 * time.Second

// reconnect marks a stdio server's death and retries with capped exponential
// backoff until it is back, the manager closes, or a manual disconnect/connect
// resolves it. Tools are deregistered while down so the model never calls into a
// dead process; on success connect() re-registers them restoring the pre-death
// enabled set.
func (m *Manager) reconnect(s *server) {
	keep := toSet(m.opts.Registrar.EnabledNames(s.source)) // registrar call stays off s.mu
	s.mu.Lock()
	c := s.c
	if c == nil { // already disconnected by /mcp disconnect or another path
		s.mu.Unlock()
		return
	}
	s.reopenKeep = keep
	s.down = true
	s.failures = 1
	s.c = nil
	s.mu.Unlock()
	_ = c.Close() // sweep the dead child's process group
	m.opts.Registrar.Unregister(s.source)
	m.updateStatus() // a dead server's tools drop out of the ratio while it is down
	s.note("server exited; reconnecting", true)

	for attempt := 2; ; attempt++ {
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(reconnectDelay(attempt - 1)):
		}
		if m.settled(s) { // a manual /mcp connect or disconnect resolved it meanwhile
			return
		}
		s.mu.Lock()
		s.failures = attempt
		s.mu.Unlock()
		if err := m.connect(m.ctx, s.name); err == nil {
			return // connect re-registered tools and started a fresh watcher
		}
	}
}

// settled reports whether some other path (manual connect or disconnect) has taken
// the server out of the reconnect loop.
func (m *Manager) settled(s *server) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.down || s.c != nil
}

// reconnectDelay is the capped exponential backoff before retry n (0-based). The
// loop starts at n=1, so the real sequence is 1s, 2s, 4s, ... up to maxReconnectWait.
func reconnectDelay(n int) time.Duration {
	d := 125 * time.Millisecond << uint(min(n+2, 9))
	if d > maxReconnectWait {
		return maxReconnectWait
	}
	return d
}

// note records a line in the server's log and surfaces it as a notice.
func (s *server) note(msg string, warn bool) {
	s.logs.add(fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), msg))
	if s.notice != nil {
		s.notice(msg, warn)
	}
}

// diag records a line only in /mcp logs; routine diagnostics stay out of history.
func (s *server) diag(msg string) {
	s.logs.add(fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), msg))
}

// RefreshStatus republishes the mcp status segment after a tool-set change made
// outside the manager (e.g. /tools toggling). Active counts non-disabled tools.
func (m *Manager) RefreshStatus() { m.updateStatus() }

// updateStatus recomputes and pushes the ratio <active>/<discovered> across every
// configured server, or clears the segment when none are configured. Active is how
// many real MCP tools are currently presented to the agent (minus any disabled by
// config or /tools); discovered is how many are registered.
func (m *Manager) updateStatus() {
	if m.opts.Status == nil {
		return
	}
	names := m.ServerNames()
	if len(names) == 0 {
		m.opts.Status("") // nothing configured; clear any stale segment
		return
	}
	var active, discovered int
	for _, name := range names {
		s := m.serverByName(name)
		if s == nil || m.opts.Registrar == nil {
			continue
		}
		registered := m.opts.Registrar.AllNames(s.source)
		disabled := toSet(m.opts.Registrar.DisabledNames(s.source))
		discovered += len(registered)
		for _, n := range registered {
			if !has(disabled, n) {
				active++
			}
		}
	}
	m.opts.Status(fmt.Sprintf("mcp: %d/%d", active, discovered))
}
