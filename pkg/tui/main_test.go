package tui

import (
	"os"
	"testing"
	"time"
)

// TestMain shortens the escape disambiguation window. Tests write whole escape
// sequences in one call, so only a deliberately lone escape ever waits it out,
// and the real value would cost every cancellation test 30ms.
func TestMain(m *testing.M) {
	escTimeout = time.Millisecond
	os.Exit(m.Run())
}
