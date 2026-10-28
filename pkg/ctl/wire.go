package ctl

import "encoding/json"

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
}

// Session names the transcript a controller attaches to.
type Session struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
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

// UserPromptData echoes a prompt's words, replay only: a live submission
// arrives as an echo event instead.
type UserPromptData struct {
	Text string `json:"text"`
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

// UsageData carries one usage report from the stream.
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

// EchoData echoes one committed user message.
type EchoData struct {
	Text string `json:"text"`
}

// ImageData is one committed image beside a prompt; Data carries the encoded
// bytes the terminal surface holds.
type ImageData struct {
	Data      []byte `json:"data,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Label     string `json:"label,omitempty"`
	W         int    `json:"w,omitempty"`
	H         int    `json:"h,omitempty"`
}

// EchoImagesData echoes the images committed beside a prompt.
type EchoImagesData struct {
	Images []ImageData `json:"images"`
}

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

// ActivityData is one keyed activity row; empty Text reports a removal.
type ActivityData struct {
	Key  string `json:"key"`
	Text string `json:"text"`
	Rank int    `json:"rank"`
}

// QueuedData replaces the pending-prompt rows, oldest first.
type QueuedData struct {
	Texts []string `json:"texts"`
}

// EditorData is the coalesced editor draft. Droppable: controllers work from
// submitted lines alone.
type EditorData struct {
	Text string `json:"text"`
}

// HeadData reports the session head moving (a rewind or fork); observers
// re-read the branch rather than guessing.
type HeadData struct {
	ID string `json:"id"`
}

// InteractionPendingData announces one open dialog and how it may be answered.
// The shapes describe what the dialog accepts, so a resolver needs no per-kind
// rules: an index answer selects from Options when present, indexes replaces
// the choice when Multi is set, text answers in free text when Text is set,
// Cancel works everywhere. The event says a decision exists; free-text answers
// never echo here.
type InteractionPendingData struct {
	ID      uint64   `json:"id"`
	Kind    string   `json:"kind"` // display hint: pick, multipick, select, confirm, input, secret, ask, decision
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

// SubmitParams routes one line exactly as typed in the editor.
type SubmitParams struct {
	Line string `json:"line"`
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

// TranscriptParams bounds a transcript.entries reply with an entry id cursor:
// only entries after it are returned.
type TranscriptParams struct {
	After string `json:"after,omitempty"`
}

// TranscriptResult is the current branch, oldest first, plus its head entry.
type TranscriptResult struct {
	Head    string  `json:"head"`
	Entries []Entry `json:"entries"`
}

// ReplayParams asks for the events past a seq the client already holds.
type ReplayParams struct {
	Since uint64 `json:"since"`
}

// ReplayResult carries the retained events past Since, in seq order.
type ReplayResult struct {
	Events []Event `json:"events"`
}

// State is the state.get result: one consistent read of everything a
// controller polls.
type State struct {
	Session     Session       `json:"session"`
	Workspace   string        `json:"workspace"`
	Cwd         string        `json:"cwd"`
	Model       ModelState    `json:"model"`
	Permission  string        `json:"permission"` // barrier mode name, "" when no gate exists
	Tools       []ToolState   `json:"tools"`
	Usage       UsageState    `json:"usage"`
	Context     ContextData   `json:"context"`
	Running     bool          `json:"running"`
	Idle        bool          `json:"idle"`
	Spinning    bool          `json:"spinning"`
	Queued      int           `json:"queued"`
	QueuedTexts []string      `json:"queuedTexts"`
	Activity    []ActivityRow `json:"activity"`
	Status      StatusRow     `json:"status"`
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

// ActivityRow is one keyed activity row as a snapshot read it.
type ActivityRow struct {
	Key  string `json:"key"`
	Text string `json:"text"`
	Rank int    `json:"rank"`
}

// StatusRow is the status line the local terminal renders.
type StatusRow struct {
	Spinner    string    `json:"spinner,omitempty"`
	Model      string    `json:"model,omitempty"`
	ModelShort string    `json:"modelShort,omitempty"`
	Tokens     int       `json:"tokens"`
	MaxTokens  int       `json:"maxTokens"`
	Reserve    int       `json:"reserve"`
	Compact    int       `json:"compact"`
	Estimated  bool      `json:"estimated"`
	Segments   []Segment `json:"segments"`
}
