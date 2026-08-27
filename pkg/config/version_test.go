package config

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		injected string
		bi       *debug.BuildInfo
		want     string
	}{
		{"ldflags_wins", "v1.2.3", &debug.BuildInfo{Main: debug.Module{Version: "v0.9.0"}}, "v1.2.3"},
		{"go_install_module_version", "dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.9.0"}}, "v0.9.0"},
		{"devel_stays_dev", "dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev"},
		{"empty_stays_dev", "dev", &debug.BuildInfo{}, "dev"},
		{"nil_build_info", "dev", nil, "dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveVersion(tc.injected, tc.bi))
		})
	}
}
