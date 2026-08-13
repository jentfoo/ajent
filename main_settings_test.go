package main

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/stretchr/testify/assert"
)

// reasoningModel is a model that supports every standard level.
func reasoningModel() llm.Model {
	m := llm.Model{Provider: "p", ID: "m"}
	m.Caps.Reasoning = true
	return m
}

func TestReasoningFromConfig(t *testing.T) {
	rc := reasoningFrom(config.Reasoning{Level: "high", Retain: "none", Show: false}, reasoningModel())
	assert.Equal(t, llm.LevelHigh, rc.Level)
	assert.Equal(t, llm.RetainNone, rc.Retain)
	assert.False(t, rc.Show)

	// an empty block falls back to the compiled-in defaults
	d := reasoningFrom(config.Reasoning{}, reasoningModel())
	assert.Equal(t, llm.LevelMedium, d.Level)
	assert.Equal(t, llm.RetainWholeTurn, d.Retain)
}

func TestToolLimitsFromConfig(t *testing.T) {
	l := toolLimitsFrom(config.ToolLimits{Bash: config.Limit{Lines: 10}, Read: config.Limit{Bytes: 4096}})
	// each configured axis copies straight through; unset axes stay zero here,
	// and ApplyLimits fills them from the package defaults at startup.
	assert.Equal(t, tools.Limit{Lines: 10}, l.Bash)
	assert.Equal(t, tools.Limit{Bytes: 4096}, l.Read)
}
