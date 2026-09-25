package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file implements the panic-isolation rule from architecture doc ch. 3.2,
// which the task book marks as the code-review red line:
//
//	Engine core goroutines must NOT recover() from a panic. A panic there means
//	the core's invariants are already broken, so continuing would corrupt state
//	silently. The process must record a crash dump and exit, handing control to
//	the Supervisor for a clean restart.
//
//	recover() is permitted only at these four boundaries:
//	  - one tool goroutine
//	  - one worker request
//	  - a non-critical background task
//	  - an RPC handler
//
// The two halves are implemented differently on purpose:
//
//	RunCore      recover-free. It redirects the runtime's own crash output to a
//	             dump file via debug.SetCrashOutput, so the panic dump is
//	             captured by the runtime and the panic then propagates and kills
//	             the process. No recover() appears anywhere on this path, which
//	             is what makes the red line checkable by inspection.
//
//	GuardBoundary  uses recover() at the four permitted boundaries and converts
//	             a panic into an observable error.
//
// GuardBoundary additionally refuses to be used for a core panic: passing a
// non-recoverable kind panics rather than silently swallowing a corrupted-state
// panic. The two APIs cannot be confused for one another.

// Supervisor is the process-level restart authority. Task 02 reports crashes;
// it never decides to restart itself, because a process whose core invariants
// just broke cannot be trusted to orchestrate its own recovery.
type Supervisor interface {
	// OnCorePanic is called after the crash dump is written and immediately
	// before the process aborts. The implementation persists the dump location
	// and arranges the restart.
	OnCorePanic(report types.CrashReport)
	// Abort terminates the process. It must not return in production.
	Abort(code int)
}

// PanicGuard captures crash dumps and routes them to the right authority.
type PanicGuard struct {
	recorder   types.CrashRecorder
	aborter    types.Aborter
	supervisor Supervisor
	dumpDir    string
	reportHook func(types.CrashReport)

	mu sync.Mutex
	// crashFile is the file receiving the runtime's crash output.
	crashFile *os.File
	// installed records that SetCrashOutput is active.
	installed bool
	// current describes the core goroutine currently executing, so a crash can
	// be correlated with the run it was serving.
	current coreContext
}

// coreContext identifies the core goroutine in flight.
type coreContext struct {
	Component string
	Meta      CrashMeta
	Since     time.Time
}

// GuardConfig configures a PanicGuard.
type GuardConfig struct {
	Recorder   types.CrashRecorder
	Aborter    types.Aborter
	Supervisor Supervisor
	DumpDir    string
	ReportHook func(types.CrashReport)
}

// NewPanicGuard builds a guard. A nil Recorder and nil Aborter are replaced
// with safe defaults: dumps go to the configured directory and a core panic
// terminates the process via the runtime followed by a forced abort.
func NewPanicGuard(cfg GuardConfig) *PanicGuard {
	g := &PanicGuard{
		recorder:   cfg.Recorder,
		aborter:    cfg.Aborter,
		supervisor: cfg.Supervisor,
		dumpDir:    cfg.DumpDir,
		reportHook: cfg.ReportHook,
	}
	if g.dumpDir == "" {
		g.dumpDir = "crashes"
	}
	if g.aborter == nil {
		g.aborter = types.AbortFunc(func(code int) { os.Exit(code) })
	}
	if g.recorder == nil {
		g.recorder = types.CrashRecorderFunc(func(r types.CrashReport) error {
			return WriteCrashDump(g.dumpDir, r)
		})
	}
	return g
}

// InstallCrashOutput redirects the Go runtime's crash output into a dump file.
//
// This is the recovery-free half of the core boundary: when a core goroutine
// panics, the runtime writes the panic message and every goroutine stack to
// this file instead of stderr, and the process exits non-zero. Nothing is
// recovered, so the panic cannot be swallowed by accident.
//
// The returned cleanup stops the redirection; it is only needed by tests, since
// in production the process is already gone by the time a dump is written.
func (g *PanicGuard) InstallCrashOutput() (func(), error) {
	if err := os.MkdirAll(g.dumpDir, 0o755); err != nil {
		return nil, types.WrapError(types.CodeInternal, err, "create crash dump directory %q", g.dumpDir)
	}
	name := fmt.Sprintf("core-crash-%d-%s.txt", os.Getpid(), time.Now().Format("20060102T150405"))
	path := filepath.Join(g.dumpDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, types.WrapError(types.CodeInternal, err, "open crash dump %q", path)
	}

	g.mu.Lock()
	prev := g.crashFile
	g.crashFile = f
	g.installed = true
	g.mu.Unlock()

	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		// Disable the redirection so a failed install does not leave the
		// runtime writing to a file we are about to close.
		debug.SetCrashOutput(nil, debug.CrashOptions{})
		_ = f.Close()
		return nil, types.WrapError(types.CodeInternal, err, "install crash output")
	}

	cleanup := func() {
		debug.SetCrashOutput(nil, debug.CrashOptions{})
		g.mu.Lock()
		if g.crashFile == f {
			g.crashFile = prev
			g.installed = false
		}
		g.mu.Unlock()
		_ = f.Close()
	}
	return cleanup, nil
}

// CrashDumpPath returns the path of the active crash dump file, if installed.
func (g *PanicGuard) CrashDumpPath() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.crashFile == nil {
		return ""
	}
	return g.crashFile.Name()
}

// RunCore runs a core goroutine's body under the core crash boundary.
//
// Usage, as the first statement of every core goroutine:
//
//	guard.RunCore("engine.actor", agent.CrashMeta{SessionID: id}, func() {
//	    a.loop()
//	})
//
// It registers the component and its run context so the crash artefact can be
// correlated with the run being served, then calls body. It does NOT recover: a
// panic propagates, the runtime writes the dump installed by InstallCrashOutput,
// and the process exits so the Supervisor can restart it.
func (g *PanicGuard) RunCore(component string, meta CrashMeta, body func()) {
	g.markCore(component, meta)
	defer g.clearCore()
	body()
}

// markCore records the core goroutine now executing.
func (g *PanicGuard) markCore(component string, meta CrashMeta) {
	g.mu.Lock()
	g.current = coreContext{Component: component, Meta: meta, Since: time.Now()}
	file := g.crashFile
	g.mu.Unlock()

	// Write a header into the dump file so the runtime's stack trace that may
	// follow can be attributed to a component and a run.
	if file != nil {
		fmt.Fprintf(file, "\n---- core goroutine %q started %s (run=%s session=%s state=%s round=%d) ----\n",
			component, time.Now().Format(time.RFC3339Nano),
			meta.RunID, meta.SessionID, meta.State, meta.Round)
	}
}

// clearCore records that the core goroutine finished normally.
func (g *PanicGuard) clearCore() {
	g.mu.Lock()
	component := g.current.Component
	g.current = coreContext{}
	file := g.crashFile
	g.mu.Unlock()
	if file != nil {
		fmt.Fprintf(file, "---- core goroutine %q exited normally %s ----\n",
			component, time.Now().Format(time.RFC3339Nano))
	}
}

// CoreContext returns the currently executing core goroutine, if any.
func (g *PanicGuard) CoreContext() (string, CrashMeta) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.current.Component, g.current.Meta
}

// Guard runs body at a permitted recover boundary and returns the panic value
// when one occurred.
//
// Go's recover() only takes effect when it is called directly by a function
// that is itself deferred — it does not work through a helper call. Guard
// therefore takes the body as a function and contains the recover() textually
// inside its own deferred literal, which is the only shape that actually
// catches a panic. A non-nil result means the caller must convert the panic
// into an error response and continue with a *fresh* unit of work, never resume
// the failed one.
//
// Use it for: one tool goroutine, one worker request, a non-critical background
// task, or an RPC handler. Never for a core goroutine.
func (g *PanicGuard) Guard(kind types.CrashKind, component string, meta CrashMeta, body func()) (recovered any) {
	if !kind.Recoverable() {
		// A programming error. Guard must not be used for a core panic, because
		// recovering there would swallow a corrupted-state panic.
		panic(fmt.Sprintf(
			"agent: Guard called with non-recoverable kind %q; core goroutines must use RunCore",
			string(kind)))
	}
	defer func() {
		if r := recover(); r != nil {
			g.persist(g.buildReport(kind, component, meta, r))
			recovered = r
		}
	}()
	body()
	return nil
}

// RecoverToError runs fn, converting a panic into a returned error. It is the
// convenience form of Guard for the "call a tool, get an error back" shape, and
// is what the agent loop uses around each individual tool call.
func (g *PanicGuard) RecoverToError(kind types.CrashKind, component string, meta CrashMeta, fn func() error) (err error) {
	if !kind.Recoverable() {
		panic(fmt.Sprintf(
			"agent: RecoverToError called with non-recoverable kind %q; core goroutines must use RunCore",
			string(kind)))
	}
	defer func() {
		if r := recover(); r != nil {
			g.persist(g.buildReport(kind, component, meta, r))
			err = types.NewError(types.CodePanicIsolated, "%s panicked: %v", component, r)
		}
	}()
	return fn()
}

// RecoverToResult is RecoverToError for a function that returns a value, used
// around a single tool execution. The recovered value is returned separately so
// the caller can distinguish "returned the zero value" from "panicked".
func RecoverToResult[T any](g *PanicGuard, kind types.CrashKind, component string, meta CrashMeta, fn func() T) (out T, recovered any) {
	if !kind.Recoverable() {
		panic(fmt.Sprintf(
			"agent: RecoverToResult called with non-recoverable kind %q; core goroutines must use RunCore",
			string(kind)))
	}
	defer func() {
		if r := recover(); r != nil {
			g.persist(g.buildReport(kind, component, meta, r))
			recovered = r
		}
	}()
	out = fn()
	return out, nil
}

// CrashMeta carries the context stamped onto a crash report.
type CrashMeta struct {
	RunID      string
	SessionID  string
	ToolCallID string
	State      types.RunState
	Round      int
}

// buildReport assembles a crash report, redacting the panic text so a
// credential embedded in an error can never reach the dump file.
func (g *PanicGuard) buildReport(kind types.CrashKind, component string, meta CrashMeta, r any) types.CrashReport {
	report := types.CrashReport{
		ID:         types.NewID("crash"),
		Kind:       kind,
		Component:  component,
		RunID:      meta.RunID,
		SessionID:  meta.SessionID,
		ToolCallID: meta.ToolCallID,
		Panic:      types.RedactString(fmt.Sprint(r)),
		Stack:      string(debug.Stack()),
		State:      meta.State,
		Round:      meta.Round,
		Timestamp:  time.Now(),
	}
	return report
}

// persist records a report and notifies the hook. It is best-effort by design:
// it runs while a panic is unwinding, so it must not itself fail loudly.
func (g *PanicGuard) persist(report types.CrashReport) {
	if g.recorder != nil {
		if err := g.recorder.Record(report); err == nil {
			report.Path = filepath.Join(g.dumpDir, fmt.Sprintf("%s-%s.txt",
				report.Timestamp.Format("20060102T150405"), report.ID))
		}
	}
	if g.reportHook != nil {
		g.reportHook(report)
	}
}

// WriteCrashDump persists a crash report as a text file. The dump is written to
// a temporary file and renamed, so a crash during the dump cannot leave a
// half-written file that a reader would mistake for a valid report.
func WriteCrashDump(dir string, report types.CrashReport) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create crash dump dir: %w", err)
	}
	name := fmt.Sprintf("%s-%s.txt", report.Timestamp.Format("20060102T150405"), report.ID)
	final := filepath.Join(dir, name)
	tmp := final + ".tmp"

	body := fmt.Sprintf(
		"crash id:     %s\nkind:         %s\ncomponent:    %s\nrun:          %s\nsession:      %s\ntool call:    %s\nstate:        %s\nround:        %d\ntimestamp:    %s\n\npanic:\n%s\n\nstack:\n%s\n",
		report.ID, report.Kind, report.Component, report.RunID, report.SessionID,
		report.ToolCallID, report.State, report.Round,
		report.Timestamp.Format(time.RFC3339Nano), report.Panic, report.Stack)

	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write crash dump: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finalize crash dump: %w", err)
	}
	return nil
}

// ParseCrashKind converts a string into a CrashKind, for configuration files
// and for tests asserting the enum round-trips.
func ParseCrashKind(s string) (types.CrashKind, error) {
	k := types.CrashKind(s)
	if !k.Valid() {
		return "", types.NewError(types.CodeInvalidArgument, "unknown crash kind %q", s)
	}
	return k, nil
}
