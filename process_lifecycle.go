package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// serviceProcess is shared by workers, pending results, and UI snapshots. A
// completed handle never signals its numeric PID again, even if it is reused.
type serviceProcess struct {
	pid     int
	mu      sync.Mutex
	stopped atomic.Bool
}

func (state *serviceState) retireStoppedProcess() {
	if state.process == nil || state.process.stopped.Load() {
		state.pid = 0
		state.process = nil
	}
}

func (p *serviceProcess) stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped.Load() {
		return nil
	}
	if err := stopProcessGroupContext(ctx, p.pid); err != nil {
		return err
	}
	p.stopped.Store(true)
	return nil
}

const processPipeDrainTimeout = time.Second

// Helper commands own a separate group so context cancellation also reaches
// descendants. WaitDelay bounds output draining when a descendant escapes it.
func configureHelperProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = processPipeDrainTimeout
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

type operationTracker struct {
	mu      sync.Mutex
	drainMu sync.Mutex
	cond    *sync.Cond
	running int
	closed  bool
	pending map[*serviceProcess]*serviceState
}

func newOperationTracker() *operationTracker {
	tracker := &operationTracker{pending: make(map[*serviceProcess]*serviceState)}
	tracker.cond = sync.NewCond(&tracker.mu)
	return tracker
}

func (t *operationTracker) command(run func() tea.Msg) tea.Cmd {
	return func() tea.Msg {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return nil
		}
		t.running++
		t.mu.Unlock()
		defer func() {
			t.mu.Lock()
			t.running--
			t.cond.Broadcast()
			t.mu.Unlock()
		}()
		return run()
	}
}

func (t *operationTracker) track(state *serviceState) {
	if t == nil || state == nil || state.process == nil {
		return
	}
	t.mu.Lock()
	snapshot := *state
	t.pending[state.process] = &snapshot
	t.mu.Unlock()
}

func (t *operationTracker) release(process *serviceProcess) {
	if t == nil || process == nil {
		return
	}
	t.mu.Lock()
	delete(t.pending, process)
	t.mu.Unlock()
}

func (t *operationTracker) waitAndStopPending() error {
	// A second quit must join cleanup already running outside the UI.
	t.drainMu.Lock()
	defer t.drainMu.Unlock()
	t.mu.Lock()
	t.closed = true
	for t.running > 0 {
		t.cond.Wait()
	}
	states := make([]*serviceState, 0, len(t.pending))
	for _, state := range t.pending {
		states = append(states, state)
	}
	t.pending = make(map[*serviceProcess]*serviceState)
	t.mu.Unlock()

	var errs []error
	for _, state := range states {
		if err := state.process.stop(context.Background()); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", state.config.Name, err))
			t.track(state) // Retain failed cleanup for the terminal-error fallback.
		}
	}
	return errors.Join(errs...)
}
