package agent

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/jentfoo/ajent/pkg/strutil"
)

// maxFaultDetail caps the panic value's model-visible text.
const maxFaultDetail = 200

// RecoverTool runs fn (one tool Execute) with panic conversion. On panic the
// returned result is the fault result, done is called once with it, and sink
// gets a LevelError notice naming the fault site (a cancelled ctx drops the
// notice). faulted is true only after a recovered panic, when res is the fault
// result and err is nil. Goexit is never recovered. A panic(nil) surfaces as a
// non-nil PanicNilError since Go 1.21.
func RecoverTool(ctx context.Context, done func(ToolResult), sink Sink, call ToolCall,
	fn func() (ToolResult, error)) (res ToolResult, err error, faulted bool) {
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		detail := faultDetail(p)
		res = ErrorToolResult("tool panicked: " + strutil.Clip(detail, maxFaultDetail))
		if ctx.Err() == nil {
			sink.Notice(fmt.Sprintf("tool panicked: %s (%s)", detail, faultLocation()), LevelError)
		}
		done(res)
		err = nil
		faulted = true
	}()
	res, err = fn()
	return res, err, false
}

// faultDetail renders a panic value as a short string. Stack is never included,
// only the value, rune-clipped.
func faultDetail(p any) string {
	switch v := p.(type) {
	case error:
		return v.Error()
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

// faultLocation returns the first stack frame above the recovered panic,
// skipping the panic machinery and this helper so the notice points at the
// faulting caller.
func faultLocation() string {
	for i := 1; i < 12; i++ {
		pc, _, _, ok := runtime.Caller(i)
		if !ok {
			break
		}
		fn := runtime.FuncForPC(pc)
		if fn == nil {
			continue
		}
		name := fn.Name()
		if name == "runtime.gopanic" ||
			strings.HasPrefix(name, "github.com/jentfoo/ajent/pkg/agent.RecoverTool") {
			continue
		}
		return name
	}
	return ""
}
