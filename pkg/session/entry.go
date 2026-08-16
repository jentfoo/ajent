// Package session persists an agent turn stream as an append-only JSONL
// transcript, one entry per line. The transcript is the source of truth: every
// state rebuild and resume reads it back through a branch rooted at a head id.
package session

import (
	"encoding/json"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// Type discriminates an entry so its opaque payload decodes correctly and an
// unknown type survives a round trip.
type Type string

const (
	TypeSession       Type = "session"
	TypeMessage       Type = "message"
	TypeCompaction    Type = "compaction"
	TypeModelChange   Type = "model_change"
	TypeSettingChange Type = "setting_change"
	TypeNotice        Type = "notice"
	TypeCustom        Type = "custom"
)

// sessionVersion is the transcript format. A reader refuses a larger major.
const sessionVersion = 1

// Version returns the current transcript format, for stamping a fresh SessionData.
func Version() int { return sessionVersion }

// Entry is one line of the transcript, linked to its parent for branching.
type Entry struct {
	ID       string          `json:"id"`
	ParentID string          `json:"parentId,omitempty"` // previous head this entry branches from; empty = root
	Type     Type            `json:"type"`
	TS       int64           `json:"ts"`             // unix milliseconds
	Data     json.RawMessage `json:"data,omitempty"` // opaque payload whose shape follows Type
}

// Decode unmarshals the entry payload into v.
func (e Entry) Decode(v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	return json.Unmarshal(e.Data, v)
}

// SessionData is the first entry of every session file.
type SessionData struct {
	Version   int    `json:"version"`
	Cwd       string `json:"cwd,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Model     string `json:"model,omitempty"` // llm.Model.Key()
	Branch    string `json:"branch,omitempty"`
	Commit    string `json:"commit,omitempty"`
}

// MessageData is one appended message and what the stream reported with it.
type MessageData struct {
	Message  llm.Message    `json:"message"`
	Stop     llm.StopReason `json:"stop,omitempty"` // assistant messages only
	Usage    llm.Usage      `json:"usage,omitzero"`
	Injected bool           `json:"injected,omitempty"` // system context excluded from prompt recall
}

// CompactionData records a context reduction without deleting anything.
type CompactionData struct {
	Summary          string          `json:"summary,omitempty"`
	FirstKeptEntryID string          `json:"firstKeptEntryId,omitempty"`
	Before           int             `json:"before,omitzero"`
	After            int             `json:"after,omitzero"`
	Reduce           *Reduce         `json:"reduce,omitempty"` // structural plan replayed on every rebuild
	Details          json.RawMessage `json:"details,omitempty"`
}

// ModelData records a model switch.
type ModelData struct {
	Model  string `json:"model"` // llm.Model.Key()
	Reason string `json:"reason,omitempty"`
}

// SettingData records one setting change (reasoning level, tool set, ...).
type SettingData struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// NoticeData is a user-visible notice worth replaying.
type NoticeData struct {
	Message string      `json:"message"`
	Level   agent.Level `json:"level,omitempty"`
}

// CustomData is opaque extension state that survives a resume.
type CustomData struct {
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data,omitempty"`
}
