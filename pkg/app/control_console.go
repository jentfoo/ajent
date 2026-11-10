package app

import (
	"errors"
	"strings"

	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/subagent"
)

// toolsSet reshapes the enabled tool set through the /tools apply: free
// replacement before this run's first dispatched prompt, enable-only after.
// The registry's change hook republishes the rows, ToolsChanged records the
// override, and a set that changes nothing records nothing.
func (s *controlServer) toolsSet(params []byte) (any, *ctl.Error) {
	var p ctl.ToolsSetParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	c := s.hub.consoleOf()
	reg := c.Tools()
	if reg == nil {
		return nil, ctl.Errf(ctl.CodeUnavailable, "no tools registered")
	}
	command.ApplyToolSet(c, p.Names)
	return ctl.ToolsSetResult{Rows: ctlTools(toolSnapshot(reg))}, nil
}

// modelsGet lists the registry exactly, config plus discovery, so a controller
// picks without riding a picker.
func (s *controlServer) modelsGet([]byte) (any, *ctl.Error) {
	c := s.hub.consoleOf()
	reg := c.Models()
	if reg == nil {
		return nil, ctl.Errf(ctl.CodeUnavailable, "no model registry")
	}
	models := reg.Models()
	out := make([]ctl.ModelState, 0, len(models))
	for _, m := range models {
		out = append(out, ctlModelOf(m))
	}
	return ctl.ModelsData{Active: reg.Active().Key(), Models: out}, nil
}

// modelSet switches onto one model by key, alias, id or a unique suffix.
// Session-only: the model-change entry and the session override record, the
// user's config layer never rewrites. During a running turn the switch queues
// and lands at the boundary, announced by its status.model event.
func (s *controlServer) modelSet(params []byte) (any, *ctl.Error) {
	var p ctl.ModelSetParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	name := strings.TrimSpace(p.Model)
	if name == "" {
		return nil, ctl.Errf(ctl.CodeBadRequest, "model.set needs a model name")
	}
	c := s.hub.consoleOf()
	reg := c.Models()
	if reg == nil {
		return nil, ctl.Errf(ctl.CodeUnavailable, "no model registry")
	}
	m, err := reg.Resolve(name)
	if err != nil {
		return nil, modelResolveError(name, err)
	}
	c.SetModel(m)
	return ctl.ModelSetResult{Model: ctlModelOf(m)}, nil
}

// modelResolveError maps a failed resolution onto typed codes: an unknown
// name is not-found, an ambiguous one bad-request naming its candidates.
func modelResolveError(name string, err error) *ctl.Error {
	var amb *llm.ErrAmbiguousModel
	switch {
	case errors.As(err, &amb):
		return ctl.Errf(ctl.CodeBadRequest, "%s matches %s; name one of them exactly",
			amb.Name, strings.Join(amb.Candidates, ", "))
	case errors.Is(err, llm.ErrUnknownModel):
		return ctl.Errf(ctl.CodeNotFound, "no model matches %s", name)
	default:
		return ctl.Errf(ctl.CodeBadRequest, "model.set: %v", err)
	}
}

// modeSet sets the permission mode by name through the same hook the
// /settings row drives, so the barrier flips live, the permission event
// publishes and no setting_change entry records: the mode stays run-scoped.
func (s *controlServer) modeSet(params []byte) (any, *ctl.Error) {
	var p ctl.ModeSetParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	c := s.hub.consoleOf()
	if msg := s.gateRefusal(keyPermissionMode); msg != "" {
		return nil, ctl.Errf(ctl.CodeUnavailable, "%s", msg)
	}
	name := strings.TrimSpace(p.Mode)
	m, ok := permit.ParseMode(name)
	if !ok {
		return nil, ctl.Errf(ctl.CodeBadRequest,
			"unknown mode %q; want one of allow-read, auto, auto+write, allow-all, block-all", name)
	}
	if err := c.SetSessionSetting(keyPermissionMode, m.String()); err != nil {
		return nil, ctl.Errf(ctl.CodeBadRequest, "mode.set: %v", err)
	}
	return ctl.ModeResult{Mode: m.String()}, nil
}

// settingsGet returns the /settings rows: label, dotted key, resolved value
// with its source layer, and editability with its reason. The row read joins
// the state lane, because a few rows read live agent fields the same lane
// serializes every console write through.
func (s *controlServer) settingsGet([]byte) (any, *ctl.Error) {
	c := s.hub.consoleOf()
	s.lane.lock()
	defer s.lane.unlock() // a panic mid-row-read must not wedge the lane
	rows := command.SettingRows(c)
	out := make([]ctl.SettingRow, 0, len(rows))
	for _, r := range rows {
		if reason := s.gateRefusal(r.Key); reason != "" {
			r.Editable = false
			r.Reason = reason
		}
		out = append(out, ctlSettingRow(r))
	}
	return ctl.SettingsData{Rows: out}, nil
}

// settingsSet validates and applies one setting as a session override with
// its live effects, optionally copying it to a config file layer. Every change
// records its setting_change entry except permissions.mode, which never does.
func (s *controlServer) settingsSet(params []byte) (any, *ctl.Error) {
	var p ctl.SettingsSetParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	if strings.TrimSpace(p.Key) == "" {
		return nil, ctl.Errf(ctl.CodeBadRequest, "settings.set needs a key")
	}
	if msg := s.gateRefusal(p.Key); msg != "" {
		return nil, ctl.Errf(ctl.CodeUnavailable, "%s", msg)
	}
	c := s.hub.consoleOf()
	row, err := command.SetSetting(c, p.Key, p.Value, p.Persist)
	if err != nil {
		return nil, settingSetError(err)
	}
	if reason := s.gateRefusal(row.Key); reason != "" {
		row.Editable = false
		row.Reason = reason
	}
	return ctl.SettingsSetResult{Row: ctlSettingRow(row)}, nil
}

// agentsList lists sub-agent jobs exactly as /agents list renders them, so a
// controller manages investigations without riding the prompt line. Read-only:
// it answers any connection.
func (s *controlServer) agentsList([]byte) (any, *ctl.Error) {
	a := s.agents()
	if a == nil {
		return nil, ctl.Errf(ctl.CodeUnavailable, "sub-agents not available")
	}
	jobs := a.List()
	out := make([]ctl.AgentJob, len(jobs))
	for i, j := range jobs {
		out[i] = ctl.AgentJob{ID: j.ID, Task: j.Task, Status: j.Status,
			ElapsedMS: j.Elapsed.Milliseconds()}
	}
	return ctl.AgentsListResult{Jobs: out}, nil
}

// agentsStop stops one job by id or every in-flight job with all, through the
// same seam /agents uses. A stopped job reports aborted, never done.
func (s *controlServer) agentsStop(params []byte) (any, *ctl.Error) {
	var p ctl.AgentsStopParams
	if rerr := decodeParams(params, &p); rerr != nil {
		return nil, rerr
	}
	id := strings.TrimSpace(p.ID)
	if p.All == (id != "") {
		return nil, ctl.Errf(ctl.CodeBadRequest, "agents.stop takes exactly one of id or all")
	}
	a := s.agents()
	if a == nil {
		return nil, ctl.Errf(ctl.CodeUnavailable, "sub-agents not available")
	}
	if p.All {
		return ctl.AgentsStopResult{Stopped: a.StopAll()}, nil
	}
	if err := a.Stop(id); err != nil {
		if errors.Is(err, subagent.ErrNoSuchAgent) {
			return nil, ctl.Errf(ctl.CodeNotFound, "agents.stop: %v", err)
		}
		// a finished job cannot be stopped again
		return nil, ctl.Errf(ctl.CodeUnavailable, "agents.stop: %v", err)
	}
	return ctl.AgentsStopResult{Stopped: 1}, nil
}

// agents returns the sub-agent seam /agents uses, nil when wiring is missing.
func (s *controlServer) agents() command.Agents {
	c := s.hub.consoleOf()
	if c == nil {
		return nil
	}
	return c.Agents()
}

// gateRefusal returns why key cannot be edited while no permission gate
// exists, "" when it may.
func (s *controlServer) gateRefusal(key string) string {
	if key == keyPermissionMode && s.hub.barrier == nil {
		return "no permission gate exists on this session"
	}
	return ""
}

// settingSetError maps the apply's error onto typed codes: an unknown key is
// not-found, an unavailable row unavailable, and everything a bad value or
// apply reports bad-request.
func settingSetError(err error) *ctl.Error {
	switch {
	case errors.Is(err, command.ErrUnknownSetting), errors.Is(err, llm.ErrUnknownModel):
		return ctl.Errf(ctl.CodeNotFound, "%v", err)
	case errors.Is(err, command.ErrSettingUnavailable):
		return ctl.Errf(ctl.CodeUnavailable, "%v", err)
	default: // bad value or failed apply, an ambiguous model key among them
		return ctl.Errf(ctl.CodeBadRequest, "%v", err)
	}
}

// ctlSettingRow maps one command row onto the wire shape.
func ctlSettingRow(r command.SettingRow) ctl.SettingRow {
	return ctl.SettingRow{Label: r.Label, Key: r.Key, Value: r.Value,
		Source: r.Source, Editable: r.Editable, Reason: r.Reason}
}

// ctlModelOf maps one registry model onto the wire state a listing and a
// model.set reply share, through the single hubModel-to-wire mapping.
func ctlModelOf(m llm.Model) ctl.ModelState {
	return ctlStateModel(hubModel{Key: m.Key(), Short: m.ShortName(),
		Window: m.ContextWindow, Caps: m.Caps})
}

// controlConn adapters onto the method registry shape.

func (c *controlConn) toolsSetCall(params []byte) (any, *ctl.Error) {
	return c.srv.toolsSet(params)
}

func (c *controlConn) modelsGetCall(params []byte) (any, *ctl.Error) {
	return c.srv.modelsGet(params)
}

func (c *controlConn) modelSetCall(params []byte) (any, *ctl.Error) {
	return c.srv.modelSet(params)
}

func (c *controlConn) modeSetCall(params []byte) (any, *ctl.Error) {
	return c.srv.modeSet(params)
}

func (c *controlConn) settingsGetCall(params []byte) (any, *ctl.Error) {
	return c.srv.settingsGet(params)
}

func (c *controlConn) settingsSetCall(params []byte) (any, *ctl.Error) {
	return c.srv.settingsSet(params)
}

func (c *controlConn) agentsListCall(params []byte) (any, *ctl.Error) {
	return c.srv.agentsList(params)
}

func (c *controlConn) agentsStopCall(params []byte) (any, *ctl.Error) {
	return c.srv.agentsStop(params)
}
