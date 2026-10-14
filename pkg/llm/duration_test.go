package llm

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDurationUnmarshalText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		expected time.Duration
		invalid  bool
	}{
		{"go_duration_string", "5m", 5 * time.Minute, false},
		{"explicit_zero", "0s", 0, false},
		{"negative_go_duration", "-90s", -90 * time.Second, false},
		{"bare_number_is_seconds", "30", 30 * time.Second, false},
		{"decimal_seconds", "1.5", 1500 * time.Millisecond, false},
		{"sub_millisecond", "0.0005", 500 * time.Microsecond, false},
		{"negative_bare_number", "-2.5", -2500 * time.Millisecond, false},
		{"large_finite_seconds", "9000000", 9000000 * time.Second, false},
		{"inf_is_invalid", "Inf", 0, true},
		{"signed_inf_is_invalid", "-inf", 0, true},
		{"nan_is_invalid", "NaN", 0, true},
		{"max_int64_seconds_errors", "9223372036854775808", 0, true}, // 2^63 seconds
		{"overflowing_bare_number_errors", "99999999999", 0, true},   // past int64 nanoseconds
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var d Duration
			err := d.UnmarshalText([]byte(tc.raw))
			if tc.invalid {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.expected, time.Duration(d))
		})
	}

	t.Run("unit_overflow_still_errors", func(t *testing.T) {
		var d Duration
		err := d.UnmarshalText([]byte("9999999999h"))
		assert.Error(t, err)
	})

	t.Run("non_numeric_falls_to_go_parser", func(t *testing.T) {
		var d Duration
		err := d.UnmarshalText([]byte("1h30m"))
		require.NoError(t, err)
		assert.Equal(t, 90*time.Minute, time.Duration(d))
	})
}

func TestDurationJSON(t *testing.T) {
	t.Parallel()

	t.Run("quoted_go_duration", func(t *testing.T) {
		var timeouts struct {
			Connect Duration `json:"connect"`
		}
		require.NoError(t, json.Unmarshal([]byte(`{"connect": "90s"}`), &timeouts))
		assert.Equal(t, 90*time.Second, time.Duration(timeouts.Connect))
	})

	t.Run("quoted_bare_seconds", func(t *testing.T) {
		var timeouts struct {
			Idle Duration `json:"idle"`
		}
		require.NoError(t, json.Unmarshal([]byte(`{"idle": "30"}`), &timeouts))
		assert.Equal(t, 30*time.Second, time.Duration(timeouts.Idle))
	})

	t.Run("inf_errors", func(t *testing.T) {
		var timeouts struct {
			Total Duration `json:"total"`
		}
		err := json.Unmarshal([]byte(`{"total": "inf"}`), &timeouts)
		assert.Error(t, err)
	})

	t.Run("marshal_writes_go_duration", func(t *testing.T) {
		out, err := json.Marshal(Duration(90 * time.Second))
		require.NoError(t, err)
		assert.JSONEq(t, `"1m30s"`, string(out))
	})
}
