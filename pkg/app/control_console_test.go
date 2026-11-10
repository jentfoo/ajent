package app

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/subagent"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
)

// wireControlConsole wires a driver-shaped console onto one rig: the config
// set, model registry, tool registry, barrier and recorder the
// console-backed control methods apply through.
func wireControlConsole(t *testing.T, rig *controlRig) *uiConsole {
	t.Helper()

	set, _, err := config.Load(config.Options{Workspace: config.Cwd(), Env: func(string) string { return "" }})
	require.NoError(t, err)
	reg, warnings := llm.NewRegistry(llm.File{Providers: map[string]llm.ProviderConfig{
		"test": {Models: []llm.ModelConfig{{ID: "alpha"}, {ID: "beta"}}},
	}}, nil, llm.RegistryOptions{Env: func(string) string { return "" }})
	require.Empty(t, warnings)
	toolsReg, terr := tools.Builtins(tools.Options{Cwd: t.TempDir(), SessionID: "ctl-console"})
	require.NoError(t, terr)
	rig.hub.setToolsRegistry(toolsReg)

	st := &agent.State{Model: reg.Active(), Tokens: tokens.New(reg.Active())}
	barrier := permit.NewBarrier(toolsReg.ReadOnly, permit.Options{
		Mode: permit.ModeAllowRead, ModeSet: true,
		Classifier: func(context.Context, permit.Subject) permit.Class { return permit.ClassUnsure },
		Notice:     func(string) {},
		DryRun:     func(agent.ToolCall) error { return nil },
	})
	console := &uiConsole{
		ui: rig.hub.ui, set: set, reg: reg, st: st, tools: toolsReg,
		commands: command.NewRegistry(), starts: newStartState(false),
		q: newSteerQueue(rig.hub.ui, func(int) {}, func(context.Context) {},
			func() []agent.MessageInfo { return nil }, rig.hub.queueDroppedCause),
		lane: newStateLane(), hub: rig.hub, dialogs: rig.broker, permit: barrier,
		rec: rig.rec.rec, sess: rig.rec,
	}
	console.refreshBase = func() {}
	rig.hub.setConsole(console)
	rig.hub.barrier = barrier
	return console
}

// settingEntries lists the setting_change entries the transcript recorded.
func settingEntries(t *testing.T, rig *controlRig) map[string]json.RawMessage {
	t.Helper()
	out := make(map[string]json.RawMessage)
	for _, e := range rigBranch(t, rig.rec) {
		if e.Type != session.TypeSettingChange {
			continue
		}
		var sd session.SettingData
		require.NoError(t, json.Unmarshal(e.Data, &sd))
		out[sd.Key] = sd.Value
	}
	return out
}

func TestControlToolsSet(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newServeRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()
	attachControl(t, c, "tools")
	wireControlConsole(t, rig)

	t.Run("free_replacement_before_dispatch", func(t *testing.T) {
		rp := c.call("t1", ctl.MethodToolsSet, map[string]any{"names": []string{tools.ToolRead}})
		require.Nil(t, rp.Error)
		var res ctl.ToolsSetResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.True(t, toolRow(t, res.Rows, tools.ToolRead).Enabled)
		assert.False(t, toolRow(t, res.Rows, tools.ToolBash).Enabled)

		// the reshape published and recorded exactly like a /tools pick
		ev := lastEvent(t, c.takePending(), ctl.EventTools)
		var td ctl.ToolsData
		require.NoError(t, ev.Decode(&td))
		assert.False(t, toolRow(t, td.Rows, tools.ToolBash).Enabled)
		assert.JSONEq(t, `["read"]`, string(settingEntries(t, rig)["tools.enabled"]))
	})

	t.Run("widen_only_after_dispatch", func(t *testing.T) {
		rig.hub.consoleOf().(*uiConsole).starts.dispatched.mark()

		// enabling bash widens; the reply carries both rows on
		rp := c.call("t2", ctl.MethodToolsSet, map[string]any{"names": []string{tools.ToolBash}})
		require.Nil(t, rp.Error)
		var res ctl.ToolsSetResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.True(t, toolRow(t, res.Rows, tools.ToolRead).Enabled)
		assert.True(t, toolRow(t, res.Rows, tools.ToolBash).Enabled)

		// a set naming none of the enabled tools cannot disable them: the
		// enable-only rule the /tools picker enforces by offering less
		rp = c.call("t3", ctl.MethodToolsSet, map[string]any{"names": []string{tools.ToolGrep}})
		require.Nil(t, rp.Error)
		require.NoError(t, rp.DecodeResult(&res))
		assert.True(t, toolRow(t, res.Rows, tools.ToolRead).Enabled) // still on
	})

	t.Run("no_registry_refuses", func(t *testing.T) {
		console := rig.hub.consoleOf().(*uiConsole)
		reg := console.tools
		console.tools = nil
		t.Cleanup(func() { console.tools = reg })

		rp := c.call("t4", ctl.MethodToolsSet, map[string]any{"names": []string{tools.ToolRead}})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)
	})
}

func TestControlModels(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newServeRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()
	attachControl(t, c, "models") // model.set writes; the listing stays open
	wireControlConsole(t, rig)

	t.Run("get_lists_registry", func(t *testing.T) {
		rp := c.call("m1", ctl.MethodModelsGet, map[string]any{})
		require.Nil(t, rp.Error)
		var res ctl.ModelsData
		require.NoError(t, rp.DecodeResult(&res))
		assert.Equal(t, "test/alpha", res.Active)
		require.Len(t, res.Models, 2)
		assert.Equal(t, "test/alpha", res.Models[0].Key)
		assert.Equal(t, "test/beta", res.Models[1].Key)
	})

	t.Run("set_switches_session_only", func(t *testing.T) {
		rp := c.call("m2", ctl.MethodModelSet, map[string]any{"model": "beta"})
		require.Nil(t, rp.Error)
		var res ctl.ModelSetResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.Equal(t, "test/beta", res.Model.Key)

		// the switch applied live, announced and recorded as a model change
		console := rig.hub.consoleOf().(*uiConsole)
		assert.Equal(t, "test/beta", console.st.Model.Key())
		rp = c.call("m3", ctl.MethodState, map[string]any{})
		var st ctl.State
		require.NoError(t, rp.DecodeResult(&st))
		assert.Equal(t, "test/beta", st.Model.Key)
		assert.True(t, slices.ContainsFunc(rigBranch(t, rig.rec), func(e session.Entry) bool {
			return e.Type == session.TypeModelChange
		}))
		// session-only: the override records, the user layer never rewrites
		_, src, _ := console.set.Explain("model")
		assert.Equal(t, "session", src)
	})

	t.Run("unknown_model_not_found", func(t *testing.T) {
		rp := c.call("m4", ctl.MethodModelSet, map[string]any{"model": "missing"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeNotFound, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "missing")
	})

	t.Run("ambiguous_model_bad_request", func(t *testing.T) {
		rp := c.call("m5", ctl.MethodModelSet, map[string]any{"model": "test"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "test/alpha")
		assert.Contains(t, rp.Error.Message, "test/beta")
	})

	t.Run("empty_name_bad_request", func(t *testing.T) {
		rp := c.call("m6", ctl.MethodModelSet, map[string]any{"model": "  "})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)
	})
}

func TestControlModeSet(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("sets_mode_and_publishes", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "mode")
		console := wireControlConsole(t, rig)

		rp := c.call("s1", ctl.MethodModeSet, map[string]any{"mode": "auto+write"})
		require.Nil(t, rp.Error)
		var res ctl.ModeResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.Equal(t, "auto+write", res.Mode)
		assert.Equal(t, permit.ModeAutoWrite, console.permit.Mode())

		ev := lastEvent(t, c.takePending(), ctl.EventPermission)
		var pd ctl.PermissionData
		require.NoError(t, ev.Decode(&pd))
		assert.Equal(t, "auto+write", pd.Mode)

		// run-scoped by design: no setting_change entry records the mode
		assert.NotContains(t, settingEntries(t, rig), keyPermissionMode)
	})

	t.Run("invalid_mode_bad_request", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "mode")
		wireControlConsole(t, rig)

		rp := c.call("s2", ctl.MethodModeSet, map[string]any{"mode": "sometimes"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "allow-all")
	})

	t.Run("no_gate_refuses", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "mode")
		wireControlConsole(t, rig)
		rig.hub.barrier = nil

		rp := c.call("s3", ctl.MethodModeSet, map[string]any{"mode": "auto"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)
	})
}

func TestControlSettings(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newServeRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()
	console := wireControlConsole(t, rig)
	t.Cleanup(func() { tools.SetImagesEnabled(true) }) // a set below blocks images process-wide

	t.Run("get_lists_rows", func(t *testing.T) {
		rp := c.call("g1", ctl.MethodSettingsGet, map[string]any{})
		require.Nil(t, rp.Error)
		var res ctl.SettingsData
		require.NoError(t, rp.DecodeResult(&res))
		require.NotEmpty(t, res.Rows)
		assert.Equal(t, "model", res.Rows[0].Key)
		assert.Equal(t, "Model", res.Rows[0].Label)

		keys := make([]string, 0, len(res.Rows))
		for _, r := range res.Rows {
			keys = append(keys, r.Key)
		}
		assert.NotContains(t, keys, "") // theme and reset carry no dotted key
		assert.NotContains(t, keys, "theme")
	})

	t.Run("get_marks_mode_row_without_gate", func(t *testing.T) {
		rig.hub.barrier = nil
		t.Cleanup(func() { rig.hub.barrier = console.permit })

		rp := c.call("g2", ctl.MethodSettingsGet, map[string]any{})
		require.Nil(t, rp.Error)
		var res ctl.SettingsData
		require.NoError(t, rp.DecodeResult(&res))
		var modeRow *ctl.SettingRow
		for i := range res.Rows {
			if res.Rows[i].Key == keyPermissionMode {
				modeRow = &res.Rows[i]
			}
		}
		require.NotNil(t, modeRow)
		assert.False(t, modeRow.Editable)
		assert.Contains(t, modeRow.Reason, "no permission gate")
	})

	t.Run("get_reads_without_seat", func(t *testing.T) {
		observer := dialControl(t, rig.path)
		observer.hello()

		rp := observer.call("g3", ctl.MethodSettingsGet, map[string]any{})
		require.Nil(t, rp.Error)
	})

	t.Run("set_applies_and_records", func(t *testing.T) {
		rp := c.call("s1", ctl.MethodSettingsSet,
			map[string]any{"key": "images.block", "value": true})
		require.NotNil(t, rp.Error) // an observer holds no seat: settings.set writes
		assert.Equal(t, ctl.CodeObserver, rp.Error.Code)

		attachControl(t, c, "set")
		rp = c.call("s2", ctl.MethodSettingsSet,
			map[string]any{"key": "images.block", "value": true})
		require.Nil(t, rp.Error)
		var res ctl.SettingsSetResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.Equal(t, "images.block", res.Row.Key)
		assert.Equal(t, "session", res.Row.Source)
		assert.JSONEq(t, "true", string(res.Row.Value))

		assert.True(t, console.set.Settings().Images.Block)
		assert.JSONEq(t, "true", string(settingEntries(t, rig)["images.block"]))
	})

	t.Run("set_validates", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			key   string
			value any
			code  string
		}{
			{"unknown_key_not_found", "nope.key", 1, ctl.CodeNotFound},
			{"min_steps_out_of_range", "compaction.minSteps", 99, ctl.CodeBadRequest},
			{"mode_value_invalid", "permissions.mode", "sometimes", ctl.CodeBadRequest},
			{"bool_rejects_string", "images.block", "yes", ctl.CodeBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rp := c.call(tc.name, ctl.MethodSettingsSet,
					map[string]any{"key": tc.key, "value": tc.value})
				require.NotNil(t, rp.Error)
				assert.Equal(t, tc.code, rp.Error.Code)
			})
		}
	})

	t.Run("set_unknown_model_not_found", func(t *testing.T) {
		rp := c.call("v5", ctl.MethodSettingsSet,
			map[string]any{"key": "model", "value": "missing"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeNotFound, rp.Error.Code)
	})

	t.Run("set_persists_to_layer", func(t *testing.T) {
		rp := c.call("p1", ctl.MethodSettingsSet,
			map[string]any{"key": "compaction.auto", "value": false, "persist": "user"})
		require.Nil(t, rp.Error)

		// the override lands, and beneath it the user layer carries the value
		require.NoError(t, console.set.ClearSession("compaction.auto"))
		_, src, ok := console.set.Explain("compaction.auto")
		require.True(t, ok)
		assert.Equal(t, "user", src)
	})

	t.Run("set_mode_through_settings", func(t *testing.T) {
		rp := c.call("p2", ctl.MethodSettingsSet,
			map[string]any{"key": keyPermissionMode, "value": "block-all"})
		require.Nil(t, rp.Error)
		var res ctl.SettingsSetResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.Equal(t, permit.ModeBlockAll, console.permit.Mode())
		assert.NotContains(t, settingEntries(t, rig), keyPermissionMode)
	})
}

// startControlJob spawns one investigation through the agent_start tool, the
// path a model takes, and returns its id.
func startControlJob(t *testing.T, mgr *subagent.Manager) string {
	t.Helper()
	for _, tool := range mgr.Tools() {
		if tool.Name() != "agent_start" {
			continue
		}
		res, err := tool.Execute(t.Context(), agent.ToolCall{
			Input: json.RawMessage(`{"task":"look around","instructions":""}`),
		}, agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		dets, ok := res.Details.(map[string]string)
		require.True(t, ok, "start result missing details")
		return dets["id"]
	}
	t.Fatal("agent_start not registered")
	return ""
}

// jobStatus reads one listed job's wire row by id.
func listedJob(res ctl.AgentsListResult, id string) (ctl.AgentJob, bool) {
	for _, j := range res.Jobs {
		if j.ID == id {
			return j, true
		}
	}
	return ctl.AgentJob{}, false
}

func TestControlAgents(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("unavailable_without_manager", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		wireControlConsole(t, rig) // console wired, no sub-agent manager behind it

		// list answers any connection, so its refusal is the verb's own
		rp := c.call("l", ctl.MethodAgentsList, map[string]any{})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)

		// stop needs the seat before its handler can refuse
		attachControl(t, c, "agents")
		rp = c.call("s", ctl.MethodAgentsStop, map[string]any{"all": true})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)
	})

	t.Run("list_answers_observer", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello() // no attach: list is a read like state.get
		console := wireControlConsole(t, rig)
		mgr := newHoldManager(t)
		console.agents = agentsAdapter{m: mgr}

		id := startControlJob(t, mgr)
		require.Eventually(t, func() bool {
			rp := c.call("l", ctl.MethodAgentsList, map[string]any{})
			require.Nil(t, rp.Error)
			var res ctl.AgentsListResult
			require.NoError(t, rp.DecodeResult(&res))
			j, ok := listedJob(res, id)
			return ok && j.Status == "running" && j.Task == "look around" && j.ElapsedMS >= 0
		}, 2*time.Second, 5*time.Millisecond)

		// stop still needs the seat
		rp := c.call("s", ctl.MethodAgentsStop, map[string]any{"id": id})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeObserver, rp.Error.Code)
	})

	t.Run("stop_reports_aborted_never_done", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "agents")
		console := wireControlConsole(t, rig)
		mgr := newHoldManager(t)
		console.agents = agentsAdapter{m: mgr}

		id := startControlJob(t, mgr)

		rp := c.call("u1", ctl.MethodAgentsStop, map[string]any{"id": "sub-9"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeNotFound, rp.Error.Code)

		rp = c.call("s1", ctl.MethodAgentsStop, map[string]any{"id": id})
		require.Nil(t, rp.Error)
		var res ctl.AgentsStopResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.Equal(t, 1, res.Stopped) // the job was in flight

		// /agents parity: a stopped job shows aborted and stays listed
		require.Eventually(t, func() bool {
			rp := c.call("l", ctl.MethodAgentsList, map[string]any{})
			require.Nil(t, rp.Error)
			var lst ctl.AgentsListResult
			require.NoError(t, rp.DecodeResult(&lst))
			j, ok := listedJob(lst, id)
			return ok && j.Status == "aborted"
		}, 2*time.Second, 5*time.Millisecond)

		// a finished-and-gone id is unknown, exactly as /agents stop reports
		rp = c.call("u2", ctl.MethodAgentsStop, map[string]any{"id": "sub-42"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeNotFound, rp.Error.Code)
	})

	t.Run("stop_all_counts_inflight_only", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "agents")
		console := wireControlConsole(t, rig)
		mgr := newHoldManager(t)
		console.agents = agentsAdapter{m: mgr}

		startControlJob(t, mgr)
		startControlJob(t, mgr)

		rp := c.call("sa", ctl.MethodAgentsStop, map[string]any{"all": true})
		require.Nil(t, rp.Error)
		var res ctl.AgentsStopResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.Equal(t, 2, res.Stopped)

		// stop all cancels without waiting: the aborts must land before counting again
		require.Eventually(t, func() bool {
			rp := c.call("l", ctl.MethodAgentsList, map[string]any{})
			require.Nil(t, rp.Error)
			var lst ctl.AgentsListResult
			require.NoError(t, rp.DecodeResult(&lst))
			return !slices.ContainsFunc(lst.Jobs, func(j ctl.AgentJob) bool {
				return j.Status == "running" || j.Status == "queued"
			})
		}, 2*time.Second, 5*time.Millisecond)

		// nothing left in flight: a second stop all cancels no one
		rp = c.call("sa2", ctl.MethodAgentsStop, map[string]any{"all": true})
		require.Nil(t, rp.Error)
		require.NoError(t, rp.DecodeResult(&res))
		assert.Equal(t, 0, res.Stopped)
	})

	t.Run("neither_id_nor_all_refused", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "agents")
		wireControlConsole(t, rig)

		rp := c.call("b1", ctl.MethodAgentsStop, map[string]any{})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)

		rp = c.call("b2", ctl.MethodAgentsStop, map[string]any{"id": "sub-1", "all": true})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)
	})
}
