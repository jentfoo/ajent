// Package llm provides one streaming interface over every supported model
// provider, plus the model registry and configuration that feed it.
package llm

import (
	"encoding/json"
	"fmt"
)

// Role is who produced a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// BlockType discriminates a Block, and is what the transcript stores.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockThinking   BlockType = "thinking"
	BlockToolCall   BlockType = "tool_call"
	BlockToolResult BlockType = "tool_result"
	BlockImage      BlockType = "image"
)

// BlockTypes lists every block discriminator in declaration order. Code that
// switches over block types — the JSON decoder, the token estimator — is expected
// to cover all of them, and their tests walk this list, so a new block type
// belongs here as well as in its own switch.
var BlockTypes = []BlockType{
	BlockText, BlockThinking, BlockToolCall, BlockToolResult, BlockImage,
}

// Origin records which provider, dialect and model produced a message. It is
// populated on rebuild for identity comparisons; it never reaches the transcript.
type Origin struct {
	Provider string
	Dialect  Dialect
	Model    string
}

// Message is one turn of the conversation.
type Message struct {
	Role    Role      `json:"role"`
	Content BlockList `json:"content"`

	Origin *Origin    `json:"-"` // producing endpoint identity, rebuild only
	Stop   StopReason `json:"-"` // terminating stop reason, rebuild only
}

// Block is one piece of message content. The unexported method seals the set.
type Block interface {
	blockType() BlockType
}

// BlockList is a block slice that round trips through JSON with the type
// discriminator each block needs to be decoded back.
type BlockList []Block

// TextBlock is plain text content.
type TextBlock struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"` // responses item id + phase
}

// ThinkingBlock is model reasoning. It carries every provider's replay token,
// since dropping one is what breaks multi turn reasoning.
type ThinkingBlock struct {
	Text      string          `json:"text,omitempty"`
	Signature string          `json:"signature,omitempty"` // anthropic
	Redacted  string          `json:"redacted,omitempty"`  // anthropic redacted_thinking, base64 verbatim
	ItemID    string          `json:"itemId,omitempty"`    // openai responses item id
	Encrypted string          `json:"encrypted,omitempty"` // openai encrypted_content
	Details   []byte          `json:"details,omitempty"`   // openrouter reasoning_details, verbatim
	Field     string          `json:"field,omitempty"`     // originating reasoning delta name
	Item      json.RawMessage `json:"item,omitempty"`      // full serialized responses item
}

// ToolCallBlock is a tool invocation requested by the model.
type ToolCallBlock struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResultBlock is the outcome of a tool call, sent back to the model.
type ToolResultBlock struct {
	CallID   string    `json:"callId"`
	ToolName string    `json:"toolName,omitempty"` // matching call name, for result-name providers
	Content  BlockList `json:"content"`
	Display  string    `json:"display,omitempty"` // what history shows when it differs from Content
	Details  any       `json:"details,omitempty"` // structured detail for extensions and the transcript
	IsError  bool      `json:"isError,omitempty"`

	AddedToolNames []string `json:"addedToolNames,omitempty"` // tool references materialized into tools
}

// ImageBlock is image content.
type ImageBlock struct {
	MediaType string `json:"mediaType"`
	Data      []byte `json:"data"`
}

func (TextBlock) blockType() BlockType       { return BlockText }
func (ThinkingBlock) blockType() BlockType   { return BlockThinking }
func (ToolCallBlock) blockType() BlockType   { return BlockToolCall }
func (ToolResultBlock) blockType() BlockType { return BlockToolResult }
func (ImageBlock) blockType() BlockType      { return BlockImage }

// Text returns a message holding a single text block.
func Text(role Role, text string) Message {
	return Message{Role: role, Content: BlockList{TextBlock{Text: text}}}
}

// blockEnvelope pairs a block with its discriminator on the wire.
type blockEnvelope struct {
	Type BlockType       `json:"type"`
	Data json.RawMessage `json:"data"`
}

// MarshalJSON encodes each block tagged with its type.
func (l BlockList) MarshalJSON() ([]byte, error) {
	envs := make([]blockEnvelope, len(l))
	for i, b := range l {
		data, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		envs[i] = blockEnvelope{Type: b.blockType(), Data: data}
	}
	return json.Marshal(envs)
}

// UnmarshalJSON decodes blocks written by MarshalJSON.
func (l *BlockList) UnmarshalJSON(data []byte) error {
	var envs []blockEnvelope
	if err := json.Unmarshal(data, &envs); err != nil {
		return err
	}
	out := make(BlockList, len(envs))
	for i, e := range envs {
		b, err := decodeBlock(e)
		if err != nil {
			return err
		}
		out[i] = b
	}
	*l = out
	return nil
}

func decodeBlock(e blockEnvelope) (Block, error) {
	switch e.Type {
	case BlockText:
		return unmarshalBlock[TextBlock](e.Data)
	case BlockThinking:
		return unmarshalBlock[ThinkingBlock](e.Data)
	case BlockToolCall:
		return unmarshalBlock[ToolCallBlock](e.Data)
	case BlockToolResult:
		return unmarshalBlock[ToolResultBlock](e.Data)
	case BlockImage:
		return unmarshalBlock[ImageBlock](e.Data)
	default:
		return nil, fmt.Errorf("llm: unknown block type %q", e.Type)
	}
}

func unmarshalBlock[T Block](data []byte) (Block, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}
