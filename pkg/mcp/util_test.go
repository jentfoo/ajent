package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFlexDurationUnmarshal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		raw    string
		want   time.Duration
		wanErr bool
	}{
		{"milliseconds", "600000", 10 * time.Minute, false},
		{"fractional_ms", "1500.75", 1500 * time.Millisecond, false},
		{"duration_string", `"90s"`, 90 * time.Second, false},
		{"quoted_ms_is_string", `"600000"`, 0, true},
		{"null_is_unset", "null", 0, false},
		{"negative", "-1000", 0, true},
		{"overflow_wraps", "1e18", 0, true},
		{"huge_string", "1e300", 0, true},
		{"bool_rejected", "true", 0, true},
		{"object_rejected", "{}", 0, true},
		{"bad_duration_string", `"5 fortnights"`, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var d FlexDuration
			err := json.Unmarshal([]byte(tc.raw), &d)
			if tc.wanErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, time.Duration(d))
		})
	}
}

func TestFlexDurationMarshal(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(FlexDuration(10 * time.Minute))
	require.NoError(t, err)
	assert.Equal(t, "600000", string(b))
}

func TestFlexStringsUnmarshal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		raw    string
		want   []string
		wanErr bool
	}{
		{"bare_true", "true", []string{"*"}, false},
		{"bare_false", "false", nil, false},
		{"null", "null", nil, false},
		{"array", `["a","b*"]`, []string{"a", "b*"}, false},
		{"empty_array", "[]", []string{}, false},
		{"quoted_glob", `"gh_*"`, []string{"gh_*"}, false},
		{"quoted_true_is_glob", `"true"`, []string{"true"}, false},
		{"quoted_false_is_glob", `"false"`, []string{"false"}, false},
		{"quoted_null_is_glob", `"null"`, []string{"null"}, false},
		{"number_rejected", "42", nil, true},
		{"bare_word_rejected", "yes", nil, true},
		{"array_of_numbers_rejected", "[1]", nil, true},
		{"object_rejected", "{}", nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var f FlexStrings
			err := json.Unmarshal([]byte(tc.raw), &f)
			if tc.wanErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, []string(f))
		})
	}
}
