package tools

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/stretchr/testify/assert"
)

func TestLimitsFrom(t *testing.T) {
	t.Parallel()

	l := LimitsFrom(config.ToolLimits{Bash: config.Limit{Lines: 10}, Read: config.Limit{Bytes: 4096}})
	// each configured axis copies straight through; unset axes stay zero here,
	// and ApplyLimits fills them from the package defaults at startup.
	assert.Equal(t, Limit{Lines: 10}, l.Bash)
	assert.Equal(t, Limit{Bytes: 4096}, l.Read)
}
