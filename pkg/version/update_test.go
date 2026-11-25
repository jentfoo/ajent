package version

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeUpdateCmds returns canned resolve/install results.
func fakeUpdateCmds(latest string, resErr, installErr error) updateCmds {
	return updateCmds{
		resolve: func(context.Context) (string, error) { return latest, resErr },
		install: func(context.Context) error { return installErr },
	}
}

func TestSelfUpdate(t *testing.T) {
	t.Parallel()

	t.Run("newer_version_installs", func(t *testing.T) {
		latest := "v3.4.5"
		res := selfUpdateWith(t.Context(), "v1.0.2",
			fakeUpdateCmds(latest, nil, nil))
		require.NoError(t, res.Err)
		assert.True(t, res.Installed)
		assert.Equal(t, latest, res.Latest)
	})

	t.Run("already_up_to_date", func(t *testing.T) {
		current := "v1.2.3"
		res := selfUpdateWith(t.Context(), current,
			fakeUpdateCmds(current, nil, nil))
		require.NoError(t, res.Err)
		assert.False(t, res.Installed)
	})

	t.Run("resolve_error_reported", func(t *testing.T) {
		res := selfUpdateWith(t.Context(), "v1.0.2",
			fakeUpdateCmds("", errors.New("offline"), nil))
		require.Error(t, res.Err)
		assert.False(t, res.Installed)
	})

	t.Run("install_error_reported", func(t *testing.T) {
		res := selfUpdateWith(t.Context(), "v1.0.3",
			fakeUpdateCmds("v2.5.6", nil, errors.New("go install: boom")))
		require.Error(t, res.Err)
		assert.Contains(t, res.Err.Error(), "go install: boom")
		assert.False(t, res.Installed)
		assert.Equal(t, "v2.5.6", res.Latest) // the resolve still succeeded
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

	// an unknown version reads as a dev build, like an empty one
	assert.Equal(t, "v0.0.0-dev", versionLabel(""))
}
