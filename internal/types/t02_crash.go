package types

import "time"

// CrashKind classifies what blew up, which decides what happens next: a core
// panic must reach the Supervisor and restart the process, whereas a
// contained panic is reported and execution continues.
type CrashKind string

const (
	// CrashKindCore is a panic in an Engine core goroutine (actor loop,
	// scheduler loop, engine run manager). A core panic is never recovered:
	// the process records a crash dump and exits so the Supervisor can restart
	// it (architecture doc ch. 3.2). Recovery here would leave the actor's
	// invariants broken, which is worse than a restart.
	CrashKindCore CrashKind = "core"

	// CrashKindTool is a panic inside one tool goroutine. It is contained and
	// converted into a failed ToolResult so the round can continue.
	CrashKindTool CrashKind = "tool"

	// CrashKindWorker is a panic inside one worker request.
	CrashKindWorker CrashKind = "worker"

	// CrashKindBackground is a panic in a non-critical background task
	// (knowledge extraction, cache diagnostics, metrics flush).
	CrashKindBackground CrashKind = "background"

	// CrashKindRPC is a panic at a request boundary (IPC handler, HTTP
	// handler). It is converted into an error response.
	CrashKindRPC CrashKind = "rpc"
)

// Core reports whether this kind must terminate the process.
func (k CrashKind) Core() bool { return k == CrashKindCore }

// Recoverable reports whether a panic of this kind may be contained by a
// recover() at its boundary. Only non-core kinds may.
func (k CrashKind) Recoverable() bool { return !k.Core() }

// Valid reports whether k is a known crash kind.
func (k CrashKind) Valid() bool {
	switch k {
	case CrashKindCore, CrashKindTool, CrashKindWorker, CrashKindBackground, CrashKindRPC:
		return true
	default:
		return false
	}
}

// CrashReport is the crash dump written when a goroutine panics. For core
// panics it is the last artifact the Engine produces before exiting: it must
// therefore contain enough context to diagnose the failure offline, but no
// credentials.
type CrashReport struct {
	ID         string    `json:"id"`
	Kind       CrashKind `json:"kind"`
	Component  string    `json:"component"`
	RunID      string    `json:"runId,omitempty"`
	SessionID  string    `json:"sessionId,omitempty"`
	ToolCallID string    `json:"toolCallId,omitempty"`
	// Panic is the formatted panic value.
	Panic string `json:"panic"`
	// Stack is the captured goroutine stack.
	Stack string `json:"stack"`
	// State is the run state at crash time, so recovery knows what phase the
	// run died in.
	State     RunState  `json:"state,omitempty"`
	Round     int       `json:"round,omitempty"`
	Timestamp time.Time `json:"ts"`
	// Path is where the dump was persisted, when it was.
	Path string `json:"path,omitempty"`
}

// CrashRecorder receives crash reports. The engine writes a report before it
// terminates the process; an implementation is expected to be synchronous and
// durable enough that the Supervisor can find the dump after the restart.
type CrashRecorder interface {
	// Record persists the report. It should not return an error for a
	// best-effort sink; the engine ignores the result because it is already
	// on its way out.
	Record(report CrashReport) error
}

// CrashRecorderFunc adapts a function to CrashRecorder.
type CrashRecorderFunc func(CrashReport) error

// Record implements CrashRecorder.
func (f CrashRecorderFunc) Record(r CrashReport) error { return f(r) }

// Aborter terminates the process after a core panic. It is an interface so
// tests can observe the abort instead of killing the test binary: asserting
// "the engine asked to be restarted" is the only practical way to test the
// panic-isolation red line in-process.
type Aborter interface {
	// Abort terminates the process with the given exit code. It must not
	// return in production; a test double may return so the test can continue.
	Abort(code int)
}

// AbortFunc adapts a function to Aborter.
type AbortFunc func(code int)

// Abort implements Aborter.
func (f AbortFunc) Abort(code int) { f(code) }

// ExitCodeCorePanic is the exit status used for a core-goroutine panic. The
// Supervisor treats it as "restart me", distinct from a clean shutdown.
const ExitCodeCorePanic = 70
