package version

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeUpdateCmds returns canned resolve/install results and records whether
// install ran.
func fakeUpdateCmds(latest string, resErr error) updateCmds {
	var installed atomic.Bool
	return updateCmds{
		resolve: func(context.Context) (string, error) { return latest, resErr },
		install: func(context.Context) error {
			installed.Store(true)
			return nil
		},
	}
}

func TestSelfUpdate(t *testing.T) {
	t.Parallel()

	t.Run("newer_version_installs", func(t *testing.T) {
		latest := "v3.4.5"
		res := selfUpdateWith(context.Background(), "v1.0.2",
			fakeUpdateCmds(latest, nil))
		require.NoError(t, res.Err)
		assert.True(t, res.Installed)
		assert.Equal(t, latest, res.Latest)
	})

	t.Run("already_up_to_date", func(t *testing.T) {
		current := "v1.2.3"
		res := selfUpdateWith(context.Background(), current,
			fakeUpdateCmds(current, nil))
		require.NoError(t, res.Err)
		assert.False(t, res.Installed)
	})

	t.Run("resolve_error_reported", func(t *testing.T) {
		res := selfUpdateWith(context.Background(), "v1.0.2",
			fakeUpdateCmds("", errors.New("offline")))
		require.Error(t, res.Err)
		assert.False(t, res.Installed)
	})

	t.Run("install_error_reported", func(t *testing.T) {
		res := selfUpdateWith(context.Background(), "v1.0.3",
			fakeUpdateCmds("v2.5.6", errors.New("offline")))
		require.Error(t, res.Err)
		assert.False(t, res.Installed)
	})
}

func TestUpdateResultNotice(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		res  UpdateResult
		want string
	}{
		{
			"updated_from_to",
			UpdateResult{Current: "v1.0.4", Latest: "v2.3.7", Installed: true},
			"updated ajent from v1.0.4 to v2.3.7",
		},
		{
			"already_up_to_date",
			UpdateResult{Current: "v1.2.3", Latest: "v1.2.3"},
			"ajent is already up to date (v1.2.3)",
		},
		{
			"dev_build_is_labelled",
			UpdateResult{Current: "dev", Latest: "dev"},
			"ajent is already up to date (v0.0.0-dev)",
		},
		{
			"error_printed",
			UpdateResult{Err: errors.New("go install: boom")},
			"update failed: go install: boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.res.Notice())
		})
	}
}

func TestVersionLabel(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"v6.7.8", "v6.7.8"},
		{"dev", "v0.0.0-dev"},
		{"", "v0.0.0-dev"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, versionLabel(tc.in))
	}
}
