package ctl

import (
	"encoding/json"
	"fmt"
	"time"
)

// Hello is the first line after accept: what this run is and where it is.
type Hello struct {
	Protocol    int     `json:"protocol"`
	Session     Session `json:"session"`
	Workspace   string  `json:"workspace"`
	Cwd         string  `json:"cwd"`
	Interactive bool    `json:"interactive"` // a local terminal renders the session
	Serve       bool    `json:"serve"`       // the socket is the only front end
	PID         int     `json:"pid"`
	Version     string  `json:"version"`
	// TranscriptPath locates the recorded transcript on disk, empty when
	// recording is off. Controllers backfill from this file: bulk catch-up
	// never rides the socket.
	TranscriptPath string `json:"transcriptPath,omitempty"`
	// Token is the serving instance's writer token, so a reconnecting
	// controller can prefer the same instance.
	Token string `json:"token,omitempty"`
}

// Session names the transcript a controller attaches to.
type Session struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Instance is one live serving instance's discovery record: written by the
// server beside its socket after listen and rewritten on seat changes and
// session renames, so controllers pick without dialing. Fields are additive
// only, so older binaries skip unknown ones.
type Instance struct {
	SessionID   string `json:"sessionID"`
	SessionName string `json:"sessionName,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	Model       string `json:"model,omitempty"`
	StartedAt   int64  `json:"startedAt"`
	PID         int    `json:"pid"`
	Token       string `json:"token"`
	Socket      string `json:"socket"`
	SeatHeld    bool   `json:"seatHeld,omitempty"`
}

// Label names the instance's session the way --resume takes it back.
func (in Instance) Label() string {
	if in.SessionName != "" {
		return in.SessionName
	}
	return in.SessionID
}

// Started returns when the instance came up.
func (in Instance) Started() time.Time {
	if in.StartedAt == 0 {
		return time.Time{}
	}
	return time.UnixMilli(in.StartedAt)
}

// String renders one listing row: label, model and pid as far as each is known.
func (in Instance) String() string {
	model := in.Model
	if model == "" {
		model = "-"
	}
	return fmt.Sprintf("%s  %s  pid %d", in.Label(), model, in.PID)
}

// --- stream event payloads, one per agent sink call ---

// TurnStartData opens a turn. Model is the active model key.
type TurnStartData struct {
	Model string `json:"model"`
}

// TextData carries one streamed delta of text or thinking.
type TextData struct {
	Delta string `json:"delta"`
}

// ToolStartData opens one tool call. Input is the raw call arguments.
type ToolStartData struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
	Label string          `json:"label,omitempty"`
	Full  bool            `json:"full,omitempty"`
}

// ToolOutputData streams one call's live output.
type ToolOutputData struct {
	ID    string `json:"id"`
	Delta string `json:"delta"`
}

// ToolProgressData reports a call whose arguments are still streaming.
type ToolProgressData struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Path  string `json:"path,omitempty"`
	Bytes int    `json:"bytes,omitempty"`
	Lines int    `json:"lines,omitempty"`
	Done  bool   `json:"done,omitempty"`
}

// ToolResultData closes one tool call with its final output and error flag.
type ToolResultData struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Output string `json:"output,omitempty"`
	Error  bool   `json:"error,omitempty"`
}

// DiffData reports one rendered file change.
type DiffData struct {
	Path   string `json:"path"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// UsageData carries one usage report, the shape turn.end and the usage
// totals share.
type UsageData struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	Reasoning  int `json:"reasoning"`
}

// ContextData reports how full the next request is.
type ContextData struct {
	Used      int  `json:"used"`
	Window    int  `json:"window"`
	Reserve   int  `json:"reserve"`
	Compact   int  `json:"compact"`
	Estimated bool `json:"estimated"`
}

// NoticeData is one notice line. Key is set for keyed notices, which a client
// replaces rather than stacks; empty for unkeyed ones.
type NoticeData struct {
	Text  string `json:"text"`
	Level string `json:"level"` // info, warn or error
	Key   string `json:"key,omitempty"`
}

// TurnEndData closes a turn.
type TurnEndData struct {
	Stop  string    `json:"stop"`
	Steps int       `json:"steps"`
	Usage UsageData `json:"usage"`
	Err   string    `json:"error,omitempty"` // transport or context failure message
}

// --- session event payloads ---

// PrintData is one markdown block, the form /help and /usage use.
type PrintData struct {
	Markdown string `json:"markdown"`
}

// EntryData announces one transcript entry appended after attach. Entries
// carry parent ids, so a client builds the session tree itself.
type EntryData struct {
	Entry Entry `json:"entry"`
}

// Entry is one transcript line: the append-only record a controller
// reconciles from.
type Entry struct {
	ID       string          `json:"id"`
	ParentID string          `json:"parentId,omitempty"`
	Type     string          `json:"type"`
	TS       int64           `json:"ts"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// StatusData is one status-row change: a keyed segment, the model labels or
// the working spinner. Exactly one field is set.
type StatusData struct {
	Segment  *Segment    `json:"segment,omitempty"`
	Model    *ModelState `json:"model,omitempty"`
	Spinning *bool       `json:"spinning,omitempty"`
}

// Segment is one keyed status segment; empty Text reports a removal.
type Segment struct {
	Key   string `json:"key"`
	Text  string `json:"text"`
	Short string `json:"short,omitempty"`
	Order int    `json:"order"`
}

// ActivityData publishes one keyed activity row as an event; it is an alias of
// ActivityRow.
type ActivityData = ActivityRow

// PermissionData announces the mode now in effect, so a cycle or a settings
// change reaches controllers without a resnapshot. The mode's status segment
// rides EventStatus beside it.
type PermissionData struct {
	Mode string `json:"mode"`
}

// ToolsData replaces the toggleable /tools rows, so every reshape (a toggle,
// an MCP connect, a plan scope) reaches controllers without polling.
type ToolsData struct {
	Rows []ToolState `json:"rows"`
}

// IdleData reports the prompt's rest state flipping: what local keys mean,
// false from dispatch until the drain settles even between turns.
type IdleData struct {
	Idle bool `json:"idle"`
}

// QueueRow is one pending prompt as the wire sees it: the stable id its row
// keeps while queued plus the typed line, redacted for secret rows where only
// the id survives.
type QueueRow struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// QueuedData replaces the pending-prompt rows, oldest first. Secret-prefixed
// rows keep their id but lose their text.
type QueuedData struct {
	Rows []QueueRow `json:"rows"`
}

// BranchChangedData reports the session head moving (a rewind or fork), the
// committed family's head event beside entry.append: from names the head left,
// head the one now current, cause why it moved (rewind, fork). No other event
// may move a head.
type BranchChangedData struct {
	From  string `json:"from,omitempty"`
	Head  string `json:"head"`
	Cause string `json:"cause,omitempty"`
}

// QueueDroppedData names the queued prompts a removal cleared, Cause saying
// which ("interrupt", "rewind" or "recalled"). Rows carry the dropped ids
// beside their texts; secret-prefixed texts redact like queue rows, but the id
// always travels so its pending row resolves.
type QueueDroppedData struct {
	Rows  []QueueRow `json:"rows"`
	Cause string     `json:"cause,omitempty"`
}

// SessionRenamedData announces a rename, so controllers and pickers learn it
// without polling. The committed truth rides entry.append of the rename entry.
type SessionRenamedData struct {
	Name string `json:"name"`
}

// InteractionPendingData announces one open dialog and how it may be answered.
// The shapes describe what the dialog accepts, so a resolver needs no per-kind
// rules: an index answer selects from Options when present, indexes replaces
// the choice when Multi is set, text answers in free text when Text is set,
// Cancel works everywhere. The event says a decision exists; free-text answers
// never echo here.
type InteractionPendingData struct {
	ID      uint64   `json:"id"`
	Kind    string   `json:"kind"` // pick, multipick, select, confirm, input, secret, ask, decision
	Prompt  string   `json:"prompt"`
	Subject string   `json:"subject,omitempty"`
	Options []string `json:"options,omitempty"` // choice labels an index answer selects from
	Multi   bool     `json:"multi,omitempty"`   // an indexes answer selects many options at once
	Text    bool     `json:"text,omitempty"`    // a text answer is accepted
	Secret  bool     `json:"secret,omitempty"`  // the text answer, never echoed anywhere
	Chat    bool     `json:"chat,omitempty"`    // a text answer may be marked as an own-words reply
}

// InteractionSettledData closes one dialog with its outcome, so no client
// holds a dialog that never resolves. Secret text never appears.
type InteractionSettledData struct {
	ID        uint64 `json:"id"`
	Kind      string `json:"kind"`
	Answered  bool   `json:"answered"`
	Cancelled bool   `json:"cancelled"`
	Index     int    `json:"index,omitempty"`
	Indexes   []int  `json:"indexes,omitempty"`
	Text      string `json:"text,omitempty"`
	Chat      bool   `json:"chat,omitempty"`
}

// RefusedData explains why a connection was closed instead of greeted. It
// rides an event because there is no request id to answer; the reason names
// what the kernel-verified credential check saw.
type RefusedData struct {
	Reason string `json:"reason"`
}

// SessionExitData is the last event before the socket closes.
type SessionExitData struct {
	Reason string `json:"reason,omitempty"`
}

// --- request params and results ---

// AttachParams claims the control seat when Control is set. Holder names the
// controller for refusal errors naming it; empty answers "controller". Force
// takes the seat from a live holder, whose writes refuse naming the new one.
type AttachParams struct {
	Control bool   `json:"control,omitempty"`
	Holder  string `json:"holder,omitempty"`
	Force   bool   `json:"force,omitempty"` // take over even when another controller holds it
}

// AttachResult reports the connection's seat state after attach.
type AttachResult struct {
	Control bool `json:"control"` // this connection holds the seat
}

// SubmitParams routes one line exactly as typed in the editor. PendingID, when
// set, is the submitting surface's correlation: it names the prompt's queue row
// and rides the committed message entry, so drops and commits resolve the
// pending row by id instead of text.
type SubmitParams struct {
	Line      string `json:"line"`
	PendingID string `json:"pendingId,omitempty"`
}

// InterruptResult names what an interrupt stopped: turn, survey, shell or
// empty when nothing ran.
type InterruptResult struct {
	Stopped string `json:"stopped,omitempty"`
}

// ModeCycleParams steps the permission mode backwards when Back is set.
type ModeCycleParams struct {
	Back bool `json:"back,omitempty"`
}

// ModeResult names the mode now in effect, empty when no gate exists.
type ModeResult struct {
	Mode string `json:"mode,omitempty"`
}

// ModeSetParams sets the permission mode by name rather than stepping it.
type ModeSetParams struct {
	Mode string `json:"mode"`
}

// InputSetParams prefills the editor buffer.
type InputSetParams struct {
	Text string `json:"text"`
}

// RewindParams rewinds onto one earlier transcript entry.
type RewindParams struct {
	Head string `json:"head"` // entry id from the transcript tree
}

// RewindResult names the head entry the session now sits on.
type RewindResult struct {
	Head string `json:"head"`
}

// RecallResult carries the recalled queued prompt back to the calling surface's
// own editor, Alt+Up over the wire. ID names the queue row that left, so a
// controller applying the watermark rule (which supersedes the recall's own
// queue.dropped event) still resolves its pending row. Empty text means nothing
// was queued, like interrupt naming nothing stopped.
type RecallResult struct {
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
}

// ResolveParams answers one pending interaction. Exactly one answer shape
// applies, validated against the shapes the pending event announced: Index for
// choices, Indexes when Multi is set (an empty list confirms no selection),
// Text when Text is set (Chat marks an ask replied in its own words), Cancel
// for Esc everywhere.
type ResolveParams struct {
	ID      uint64  `json:"id"`
	Index   *int    `json:"index,omitempty"`
	Indexes *[]int  `json:"indexes,omitempty"` // pointer so an empty list is expressible
	Text    *string `json:"text,omitempty"`
	Chat    bool    `json:"chat,omitempty"`
	Cancel  bool    `json:"cancel,omitempty"`
}

// ToolsSetParams reshapes the enabled tool set: a free replacement before
// this run's first dispatched prompt, enable-only after, the same rule the
// /tools pickers apply. Unknown names are ignored, and after the first prompt
// names left out keep their state rather than disabling anything.
type ToolsSetParams struct {
	Names []string `json:"names"`
}

// ToolsSetResult reports the toggleable rows after the reshape, the same full
// replacement the tools event carries.
type ToolsSetResult struct {
	Rows []ToolState `json:"rows"`
}

// ModelsData lists the registry's models (config plus discovery, no compiled
// entries) with the active key, so a controller picks without riding a picker.
type ModelsData struct {
	Active string       `json:"active"`
	Models []ModelState `json:"models"`
}

// ModelSetParams switches the session onto one model resolved by key, alias,
// id or a unique suffix. The switch is session-only: the user's config layer
// is never rewritten.
type ModelSetParams struct {
	Model string `json:"model"`
}

// ModelSetResult names the model the switch resolved to. During a running
// turn the switch queues and lands at the boundary, announced by its
// status.model event.
type ModelSetResult struct {
	Model ModelState `json:"model"`
}

// SettingsData is the settings.get result: the /settings rows, one per
// editable dotted key, in menu order.
type SettingsData struct {
	Rows []SettingRow `json:"rows"`
}

// SettingRow is one /settings row as the wire carries it: its label, dotted
// key, resolved value with the layer it came from, and whether it can be
// edited right now. Reason names why not when it cannot.
type SettingRow struct {
	Label    string          `json:"label"`
	Key      string          `json:"key,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
	Source   string          `json:"source,omitempty"` // default|user|project|local|session|active
	Editable bool            `json:"editable"`
	Reason   string          `json:"reason,omitempty"`
}

// SettingsSetParams applies one setting as a session override with its live
// effects, optionally copying it to a config file layer. Value carries the
// JSON value in the row's own shape (bool, number, string, or a list of tool
// names for tools.enabled).
type SettingsSetParams struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	Persist string          `json:"persist,omitempty"` // user | project
}

// SettingsSetResult reports the row after the change.
type SettingsSetResult struct {
	Row SettingRow `json:"row"`
}

// SeatLostData announces one force takeover of the control seat: From names
// the displaced holder (empty when it never announced one), To its successor.
type SeatLostData struct {
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

// State is the state.get result: one consistent read of everything a
// controller polls. Interactions lists the dialogs still holding the run,
// exactly the shape their pending events carry, so a controller connecting
// mid-dialog learns what holds input and can answer it by id. Open dialogs
// are runtime state: they ride the socket only, never the transcript or the
// discovery record.
type State struct {
	Session      Session                  `json:"session"`
	Workspace    string                   `json:"workspace"`
	Cwd          string                   `json:"cwd"`
	Head         string                   `json:"head"` // transcript head entry id (fork point)
	Model        ModelState               `json:"model"`
	Permission   string                   `json:"permission"` // barrier mode name, "" if none
	Tools        []ToolState              `json:"tools"`
	Usage        UsageState               `json:"usage"`
	Context      ContextData              `json:"context"`
	Running      bool                     `json:"running"`
	Idle         bool                     `json:"idle"`
	Spinning     bool                     `json:"spinning"`
	Queued       int                      `json:"queued"`
	QueuedRows   []QueueRow               `json:"queuedRows"`
	Activity     []ActivityRow            `json:"activity"`
	Status       StatusRow                `json:"status"`
	Seat         SeatState                `json:"seat"`
	Interactions []InteractionPendingData `json:"interactions,omitempty"` // open dialogs, oldest first
}

// SeatState reports the control seat: whether anyone holds it, who, and
// whether this connection is the holder.
type SeatState struct {
	Held   bool   `json:"held"`
	Holder string `json:"holder,omitempty"` // announced name of the holder
	Yours  bool   `json:"yours"`
}

// ModelState is the model a snapshot read, capabilities included.
type ModelState struct {
	Key    string `json:"key"`
	Short  string `json:"short"`
	Window int    `json:"window"`
	Caps   Caps   `json:"caps"`
}

// Caps is the capability subset a controller needs.
type Caps struct {
	Reasoning         bool `json:"reasoning"`
	Images            bool `json:"images"`
	ParallelToolCalls bool `json:"parallelToolCalls"`
}

// ToolState is one toggleable /tools row.
type ToolState struct {
	Name     string `json:"name"`
	Source   string `json:"source"`
	Enabled  bool   `json:"enabled"`
	ReadOnly bool   `json:"readOnly"`
}

// UsageState is the ledger's numbers, the same terms /usage reports.
type UsageState struct {
	Turns     int                  `json:"turns"`
	Total     UsageData            `json:"total"`
	Child     UsageData            `json:"child"`
	ByModel   map[string]UsageData `json:"byModel"`
	Estimated int                  `json:"estimatedTurns"`
}

// ActivityRow is one keyed activity row (the snapshot and its event payload
// share this shape); empty Text reports a removal.
type ActivityRow struct {
	Key  string `json:"key"`
	Text string `json:"text"`
	Rank int    `json:"rank"`
}

// StatusRow is the status line's keyed segments. Model labels live in State,
// context numbers in Context, and the working state in Spinning.
type StatusRow struct {
	Segments []Segment `json:"segments"`
}
