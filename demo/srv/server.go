package srv

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// modelID is the single advertised and accepted model.
const modelID = "ajent-demo-1"

// Server is a scripted chat-completions HTTP server. It holds no per-run state:
// every run's identity lives in its transcript, so concurrent runs are safe.
type Server struct {
	ln   net.Listener
	http *http.Server
}

// Start binds addr and serves the demo routes until Close. An empty addr picks a
// free port; the caller reads URL() to learn where it landed.
func Start(addr string) (*Server, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("demo: listen %s: %w", addr, err)
	}
	s := &Server{ln: ln}
	s.http = &http.Server{Handler: s.router()}
	go func() { _ = s.http.Serve(ln) }()
	return s, nil
}

// router builds the request mux; exposed so tests drive it without binding.
func (s *Server) router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/models", s.handleModels)
	return mux
}

// URL returns the base URL a client should target (host and port only).
func (s *Server) URL() string { return "http://" + s.ln.Addr().String() }

// Close shuts the server down.
func (s *Server) Close() error {
	if s.http != nil {
		return s.http.Close()
	}
	return nil
}

// handleModels reports the single advertised model, either base-URL convention.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model","owned_by":"ajent"}]}`, modelID)
}

// handleChat serves one completion: the script for a normal request, or a
// single-word classification when no tools are advertised.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"bad request body"}}`, http.StatusBadRequest)
		return
	}
	prompt := len(marshalJSON(req)) / 4 // prompt_tokens estimate from the wire size

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	if !req.Stream {
		s.handleNonStream(w, req)
		return
	}
	wr := newWriter(w, req.Model)

	if len(req.Tools) == 0 {
		s.classify(wr, req.Model, req, prompt)
		wr.done()
		return
	}
	s.playStep(wr, req, prompt)
}

// classify answers a classifier request: no tools are advertised and a single user
// message holds the call under evaluation. It approves everything, so no barrier
// mode blocks on a call the agent asked about.
func (s *Server) classify(wr *writer, model string, req chatRequest, prompt int) {
	wr.textDelta(model, approveWord(systemText(req)))
	wr.finish(model, "stop", prompt)
}

// approveWord returns the approving verdict a classifier prompt asks for. Each
// prompt lists its categories most-permissive first, so the first word quoted
// after a bullet is the answer; a caller whose prompt says nothing gets "allow".
// Read rather than assumed, since any harness may drive this server.
func approveWord(system string) string {
	for _, line := range strings.Split(system, "\n") {
		if !strings.HasPrefix(line, `- "`) {
			continue
		}
		if end := strings.Index(line[3:], `"`); end > 0 {
			return line[3 : 3+end]
		}
	}
	return "allow"
}

// systemText joins the system messages of a request; a caps-driven client may
// label them developer instead.
func systemText(req chatRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		if s, ok := m.Content.(string); ok {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// playStep streams the script turn at req's current step index.
func (s *Server) playStep(wr *writer, req chatRequest, prompt int) {
	model := req.Model
	if model == "" {
		model = modelID
	}
	stps := script()
	idx := stepIndex(req)
	// past the end of a completed run: tell them it is over rather than crash.
	if idx >= len(stps) {
		for _, frag := range chunks("demo complete, restart to run again") {
			wr.textDelta(model, frag)
		}
		wr.finish(model, "stop", prompt)
		return
	}
	stp := stps[idx]

	var r *run
	if idx == 0 {
		r = newRun(time.Now())
	} else if rr, ok := recoverRun(req); ok {
		r = rr
	} else { // no scratch dir yet and not step 0: fall back to a fresh run
		r = newRun(time.Now())
	}

	if stp.think != "" {
		for _, frag := range chunks(stp.think) {
			wr.reasoningDelta(model, frag)
		}
	}
	content := stp.content
	if idx == len(stps)-1 { // the closing turn carries the measured run time; the full
		total := fmt.Sprintf("%.3f", time.Since(r.start).Seconds())
		fmt.Fprintf(os.Stderr, "demo run complete: %ss\n", total) // non-ajent harnesses read this
		// markdown/style showcase renders last so the text wall cannot bury it.
		content = demoReply + "\n\n" + summaryMarkdown(total)
	}
	if content != "" {
		for _, frag := range chunks(content) {
			wr.textDelta(model, frag)
		}
	}

	if len(stp.calls) == 0 { // last step ends the turn
		wr.finish(model, "stop", prompt)
		return
	}

	advertised := advertisedNames(req)
	for i, call := range stp.calls {
		name, args, ok := resolveCall(advertised, call, r)
		if !ok {
			continue // nothing resolvable; the step degrades to prose only
		}
		id := fmt.Sprintf("call_demo_%d_%d", idx, i)
		wr.toolCallOpen(model, i, id, name)
		for _, frag := range chunks(string(args)) {
			if strings.TrimSpace(frag) == "" {
				continue
			}
			wr.toolCallArgs(model, i, frag)
		}
	}
	wr.finish(model, "tool_calls", prompt)
}

// handleNonStream answers a non-stream request with a single JSON completion.
func (s *Server) handleNonStream(w http.ResponseWriter, req chatRequest) {
	stps := script()
	idx := stepIndex(req)
	w.Header().Set("Content-Type", "application/json")
	if idx >= len(stps) {
		_, _ = fmt.Fprintf(w, `{"id":"demo","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"demo complete, restart to run again"},"finish_reason":"stop"}]}`, req.Model)
		return
	}
	stp := stps[idx]
	content := stp.content
	if len(stp.calls) > 0 && content == "" {
		content = fmt.Sprintf("running %d tool call(s)", len(stp.calls))
	}
	_, _ = fmt.Fprintf(w, `{"id":"demo","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]}`,
		req.Model, marshalJSON(content))
}

// chunks splits text on whitespace, keeping the separators attached.
func chunks(text string) []string {
	var out []string
	var start int
	for i, r := range text {
		if r == ' ' || r == '\n' {
			out = append(out, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}
