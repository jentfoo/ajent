package llm

import (
	"encoding"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLevelUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected Level
	}{
		{"off", `"off"`, LevelOff},
		{"minimal", `"minimal"`, LevelMinimal},
		{"low", `"low"`, LevelLow},
		{"medium", `"medium"`, LevelMedium},
		{"high", `"high"`, LevelHigh},
		{"xhigh", `"xhigh"`, LevelXHigh},
		{"max", `"max"`, LevelMax},
		{"case_insensitive", `"MEDIUM"`, LevelMedium},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got Level
			require.NoError(t, json.Unmarshal([]byte(tc.input), &got))
			assert.Equal(t, tc.expected, got)

			data, err := json.Marshal(tc.expected)
			require.NoError(t, err)
			assert.JSONEq(t, `"`+tc.expected.String()+`"`, string(data))
		})
	}

	t.Run("unknown_names_the_options", func(t *testing.T) {
		var got Level
		err := json.Unmarshal([]byte(`"enormous"`), &got)
		require.ErrorContains(t, err, "unknown reasoning level")
		assert.Contains(t, err.Error(), "off, minimal, low, medium, high, xhigh, max")
	})
	t.Run("level_set_is_covered", func(t *testing.T) {
		// any thinkingLevelMap written against the standard levels maps every key
		var m map[Level]*string
		require.NoError(t, json.Unmarshal([]byte(
			`{"off":null,"minimal":"minimal","low":"low","medium":"medium","high":"high","xhigh":"high","max":"high"}`), &m))
		assert.Len(t, m, 7)
		assert.Nil(t, m[LevelOff])
		require.NotNil(t, m[LevelMax])
		assert.Equal(t, "high", *m[LevelMax])
	})
}

func TestParseLevel(t *testing.T) {
	t.Parallel()

	got, ok := ParseLevel("xhigh")
	require.True(t, ok)
	assert.Equal(t, LevelXHigh, got)

	_, ok = ParseLevel("nope")
	assert.False(t, ok)
}

func TestRetainPolicyUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected RetainPolicy
	}{
		{"none", `"none"`, RetainNone},
		{"last_turn", `"lastTurn"`, RetainLastTurn},
		{"whole_turn", `"wholeTurn"`, RetainWholeTurn},
		{"all", `"all"`, RetainAll},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got RetainPolicy
			require.NoError(t, json.Unmarshal([]byte(tc.input), &got))
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestReasoningStyleUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected ReasoningStyle
	}{
		{"named_style", `"anthropic_budget"`, ReasoningAnthropicBudget},
		{"inline_tags", `"inline_tags"`, ReasoningInlineTags},
		{"bool_true", `true`, ReasoningUnset},
		{"bool_false", `false`, ReasoningNone},
		{"explicit_none", `"none"`, ReasoningNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got ReasoningStyle
			require.NoError(t, json.Unmarshal([]byte(tc.input), &got))
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestTokenizerKindUnmarshalJSON(t *testing.T) {
	t.Parallel()

	var got TokenizerKind
	require.NoError(t, json.Unmarshal([]byte(`"remote_tokenize"`), &got))
	assert.Equal(t, TokenizerRemoteTokenize, got)
	assert.Equal(t, "remote_tokenize", got.String())
}

// enumRoundTrip asserts every value encodes to its name and decodes back.
func enumRoundTrip[T interface {
	~uint8
	encoding.TextMarshaler
	fmt.Stringer
}, P interface {
	*T
	encoding.TextUnmarshaler
}](t *testing.T, values []T) {
	t.Helper()

	for _, v := range values {
		data, err := v.MarshalText()
		require.NoError(t, err)
		assert.Equal(t, v.String(), string(data))

		var got T
		require.NoError(t, P(&got).UnmarshalText(data))
		assert.Equal(t, v, got)
	}
}

func TestEnumTextRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("level", func(t *testing.T) {
		enumRoundTrip[Level, *Level](t, Levels())
	})
	t.Run("retain_policy", func(t *testing.T) {
		enumRoundTrip[RetainPolicy, *RetainPolicy](t,
			[]RetainPolicy{RetainNone, RetainLastTurn, RetainWholeTurn, RetainAll})
	})
	t.Run("reasoning_style", func(t *testing.T) {
		enumRoundTrip[ReasoningStyle, *ReasoningStyle](t, []ReasoningStyle{
			ReasoningNone, ReasoningAnthropicBudget, ReasoningOpenAIEffort,
			ReasoningOpenRouter, ReasoningInlineTags, ReasoningContentField, ReasoningUnset,
		})
	})
	t.Run("tokenizer_kind", func(t *testing.T) {
		enumRoundTrip[TokenizerKind, *TokenizerKind](t, []TokenizerKind{
			TokenizerNone, TokenizerRemoteCount, TokenizerRemoteTokenize, TokenizerLocalEstimate,
		})
	})
	t.Run("unencodable_value_errors", func(t *testing.T) {
		_, err := Level(200).MarshalText()
		assert.ErrorContains(t, err, "cannot encode enum value 200")
	})
	t.Run("unknown_value_names_unknown", func(t *testing.T) {
		assert.Equal(t, "unknown", Level(200).String())
	})
}

func TestParseRetain(t *testing.T) {
	t.Parallel()

	got, ok := ParseRetain("wholeTurn")
	require.True(t, ok)
	assert.Equal(t, RetainWholeTurn, got)

	_, ok = ParseRetain("nope")
	assert.False(t, ok)
}

func TestLevels(t *testing.T) {
	t.Parallel()

	got := Levels()
	assert.Len(t, got, 7)
	assert.Equal(t, LevelOff, got[0])
	assert.Equal(t, LevelMax, got[len(got)-1])
}
