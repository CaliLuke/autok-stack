package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) >= 4 && os.Args[1] == serviceWrapperArg {
		os.Exit(runServiceWrapper(os.Args[3:]))
	}
	os.Exit(m.Run())
}

func TestLifecycleEarlyExitIsServiceScoped(t *testing.T) {
	a := &serviceState{config: serviceConfig{Key: "a"}, starting: true}
	b := &serviceState{config: serviceConfig{Key: "b"}, starting: true}
	m := testSupervisorModel(map[string]*serviceState{"a": a, "b": b}, []string{"a", "b"})
	m.bootRemaining = 2
	updated, _ := m.Update(processExitMsg{key: "a", pid: 101, generation: 1, err: errors.New("A crashed")})
	m = updated.(model)
	updated, _ = m.Update(serviceBootResultMsg{key: "b", state: &serviceState{config: b.config, pid: 102, generation: 1, running: true}})
	m = updated.(model)
	if !b.running || b.exitErr != nil {
		t.Fatalf("healthy B consumed A's failure: running=%v err=%v", b.running, b.exitErr)
	}
	m.Update(serviceBootResultMsg{key: "a", state: &serviceState{config: a.config, pid: 101, generation: 1, running: true}})
	if a.running || a.exitErr == nil || len(m.earlyExits) != 0 {
		t.Fatalf("A's failure was lost or retained: state=%+v early=%v", a, m.earlyExits)
	}
}

func TestLifecycleShutdownRetainsPendingReplacement(t *testing.T) {
	m := testSupervisorModel(map[string]*serviceState{"a": {config: serviceConfig{Key: "a"}, generation: 1}}, []string{"a"})
	m.shuttingDown = true
	cfg := serviceConfig{Key: "a", Command: []string{"sh", "-c", "exec sleep 30"}, LogFile: filepath.Join(t.TempDir(), "a.log")}
	next, err := startServiceContext(context.Background(), cfg, m.exitCh, 2, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer next.process.stop(context.Background())
	m.operations.track(next)
	m.Update(restartResultMsg{key: "a", generation: 2, next: next, oldStopped: true})
	if err := m.operations.waitAndStopPending(); err != nil {
		t.Fatal(err)
	}
	if err := shutdownServices(m.services, m.order); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(next.pid, 0); err == nil {
		t.Fatal("live replacement survived both tracker and model shutdown")
	}
}

func TestLifecycleRestartPreservesBackoffUntilHealthyWindow(t *testing.T) {
	state := &serviceState{config: serviceConfig{Key: "a", AutoRestart: true}, generation: 1, restarting: true, restartAttempt: 4}
	m := testSupervisorModel(map[string]*serviceState{"a": state}, []string{"a"})
	m.applyRestartResult(restartResultMsg{key: "a", generation: 2, next: &serviceState{config: state.config, generation: 2, running: true, startedAt: time.Now()}})
	if state.restartAttempt != 4 {
		t.Fatalf("restart reset attempt to %d before 30s healthy window", state.restartAttempt)
	}
}

func TestLifecycleFallbackStopsUndeliveredBootResult(t *testing.T) {
	cfg := serviceConfig{Key: "a", Command: []string{"sh", "-c", "exec sleep 30"}, LogFile: filepath.Join(t.TempDir(), "a.log")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker := newOperationTracker()
	services, order, results, done := beginBootServices(ctx, tracker, []serviceConfig{cfg}, make(chan processExitMsg, 4), 1024)
	<-done
	result := <-results
	defer result.state.process.stop(context.Background())
	cancel()
	if err := tracker.waitAndStopPending(); err != nil {
		t.Fatal(err)
	}
	if err := shutdownServices(services, order); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(result.state.pid, 0); err == nil {
		t.Fatal("fallback returned success but undelivered boot process is still alive")
	}
}

func TestLifecycleExitDoesNotWaitForDescendantLogEOF(t *testing.T) {
	cfg := serviceConfig{Key: "a", Command: []string{"sh", "-c", "sleep 30 & exit 7"}, LogFile: filepath.Join(t.TempDir(), "a.log")}
	registry, err := openProcessRegistry(filepath.Join(t.TempDir(), "processes.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.registry = registry
	exits := make(chan processExitMsg, 2)
	state, err := startServiceContext(context.Background(), cfg, exits, 1, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer state.process.stop(context.Background())
	select {
	case <-exits:
		if len(registry.snapshot()) != 0 {
			t.Fatal("descendants still tracked after exit")
		}
		if err := syscall.Kill(-state.pid, 0); err == nil {
			t.Fatal("descendants survived wrapper exit")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("leader exited but descendant holding stdout prevents exit notification and cleanup")
	}
}

func TestLifecycleComposeCancellationBoundsPipeWait(t *testing.T) {
	cfg := serviceConfig{ComposeCommand: []string{"sh", "-c", "sleep 1 & wait"}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _ = cfg.composeCommandContext(ctx).Output()
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("50ms cancellation returned after %s", elapsed)
	}
}

func TestLifecycleExitedPIDIsRetiredFromShutdown(t *testing.T) {
	state := &serviceState{config: serviceConfig{Key: "a"}, generation: 1, pid: 12345, running: true, process: &serviceProcess{pid: 12345}}
	state.process.stopped.Store(true)
	m := testSupervisorModel(map[string]*serviceState{"a": state}, []string{"a"})
	m.Update(processExitMsg{key: "a", pid: 12345, generation: 1})
	if len(shutdownServiceLayers(m.services, m.order)) > 0 {
		t.Fatal("already-reaped numeric PID remains a future shutdown signal target")
	}
}

func TestLifecycleConcurrentShutdownJoinsPendingCleanup(t *testing.T) {
	tracker := newOperationTracker()
	process := &serviceProcess{pid: 12345}
	// Hold cleanup at the process boundary without signaling a real PID.
	process.mu.Lock()
	tracker.track(&serviceState{process: process})
	first := make(chan error, 1)
	go func() { first <- tracker.waitAndStopPending() }()
	deadline := time.Now().Add(time.Second)
	for {
		tracker.mu.Lock()
		claimed := len(tracker.pending) == 0
		tracker.mu.Unlock()
		if claimed {
			break
		}
		if time.Now().After(deadline) {
			process.stopped.Store(true)
			process.mu.Unlock()
			t.Fatal("first cleanup did not claim pending processes")
		}
		time.Sleep(time.Millisecond)
	}
	second := make(chan error, 1)
	go func() { second <- tracker.waitAndStopPending() }()
	select {
	case <-second:
		process.stopped.Store(true)
		process.mu.Unlock()
		t.Fatal("fallback returned while earlier cleanup was still pending")
	case <-time.After(50 * time.Millisecond):
	}
	process.stopped.Store(true)
	process.mu.Unlock()
	for _, result := range []<-chan error{first, second} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("cleanup did not complete")
		}
	}
}

func TestLifecycleExitRetainsUnfinishedDescendantCleanup(t *testing.T) {
	state := &serviceState{config: serviceConfig{Key: "a"}, generation: 1, pid: 12345, process: &serviceProcess{pid: 12345}}
	m := testSupervisorModel(map[string]*serviceState{"a": state}, []string{"a"})
	m.Update(processExitMsg{key: "a", pid: 12345, generation: 1, err: errors.New("descendant cleanup failed")})
	if state.process == nil || len(shutdownServiceLayers(m.services, m.order)) != 1 {
		t.Fatal("failed descendant cleanup lost its shutdown retry")
	}
}

func TestLifecycleRetiredSnapshotCannotSignalReusedPID(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exec sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = stopProcessGroup(cmd.Process.Pid); <-done })
	// Simulate an old snapshot's numeric PID having been reused by this child.
	retired := &serviceProcess{pid: cmd.Process.Pid}
	retired.stopped.Store(true)
	state := &serviceState{config: serviceConfig{Key: "old"}, process: retired, pid: cmd.Process.Pid}
	if err := shutdownServices(map[string]*serviceState{"old": state}, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatal("retired snapshot signaled an unrelated process", err)
	}
}

func TestLifecycleUnhealthyUptimeDoesNotResetBackoff(t *testing.T) {
	state := &serviceState{running: true, restartAttempt: 4, startedAt: time.Now().Add(-time.Minute), liveFailures: 1}
	resetRestartBackoffAfterHealthy(state, time.Now())
	if state.restartAttempt != 4 {
		t.Fatal("unhealthy uptime reset crash history")
	}
	state.liveFailures = 0
	state.restarting = true
	resetRestartBackoffAfterHealthy(state, time.Now())
	if state.restartAttempt != 4 {
		t.Fatal("restart in progress reset crash history")
	}
}
