package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestAutomaticRestartWaitsForDependencies(t *testing.T) {
	tempDir := t.TempDir()
	dependency := &serviceState{config: serviceConfig{Key: "db", Name: "DB"}, exitErr: errors.New("down")}
	app := &serviceState{
		config: serviceConfig{
			Key:         "app",
			Name:        "App",
			DependsOn:   []string{"db"},
			AutoRestart: true,
			WorkDir:     tempDir,
			LogFile:     filepath.Join(tempDir, "app.log"),
			Command:     []string{"sh", "-c", "sleep 5"},
		},
		exitErr: errors.New("dependency db failed"),
	}
	m := testSupervisorModel(map[string]*serviceState{"db": dependency, "app": app}, []string{"db", "app"})
	m.healthBusy = true

	updated, _ := m.Update(tickMsg(time.Now()))
	got := updated.(model).services["app"]
	if got.restarting || got.running {
		t.Fatalf("dependency-blocked service started: running=%v restarting=%v", got.running, got.restarting)
	}
}

func TestManualRestartWaitsForDependencies(t *testing.T) {
	dependency := &serviceState{config: serviceConfig{Key: "db", Name: "DB"}, exitErr: errors.New("down")}
	app := &serviceState{config: serviceConfig{Key: "app", Name: "App", DependsOn: []string{"db"}}}
	m := testSupervisorModel(map[string]*serviceState{"db": dependency, "app": app}, []string{"app", "db"})

	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	got := updated.(model).services["app"]
	if command != nil || got.restarting {
		t.Fatalf("manual restart bypassed dependency: command=%v restarting=%v", command != nil, got.restarting)
	}
	if got.exitErr == nil || !strings.Contains(got.exitErr.Error(), "dependency db") {
		t.Fatalf("missing dependency error: %v", got.exitErr)
	}
}

func TestManualRestartRunsOutsideUpdateAndRecoversHealth(t *testing.T) {
	tempDir := t.TempDir()
	state := &serviceState{
		config: serviceConfig{
			Key:     "app",
			Name:    "App",
			WorkDir: tempDir,
			LogFile: filepath.Join(tempDir, "app.log"),
			Command: []string{"sh", "-c", "sleep 5"},
		},
		exitErr: errors.New("down"),
	}
	m := testSupervisorModel(map[string]*serviceState{"app": state}, []string{"app"})
	m.anyExited = true

	started := time.Now()
	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m = updated.(model)
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("Update blocked for %s", elapsed)
	}
	if command == nil || !m.services["app"].restarting {
		t.Fatal("restart was not scheduled asynchronously")
	}

	message := command()
	updated, _ = m.Update(message)
	m = updated.(model)
	t.Cleanup(func() { _ = shutdownServices(m.services, m.order) })
	if !m.services["app"].running || m.anyExited || m.anyAnimating() {
		t.Fatalf("successful restart did not recover health: running=%v degraded=%v animating=%v", m.services["app"].running, m.anyExited, m.anyAnimating())
	}
}

func TestComposeHealthFailureRendersUnavailable(t *testing.T) {
	state := &serviceState{
		config:  serviceConfig{Key: "db", Name: "DB", ComposeServices: []string{"db"}},
		running: true,
	}
	m := testSupervisorModel(map[string]*serviceState{"db": state}, []string{"db"})
	m.styles = newStyles()
	m.healthBusy = true

	updated, _ := m.Update(healthCheckMsg{results: []serviceHealthResult{{key: "db", running: true, err: errors.New("not ready")}}})
	m = updated.(model)
	if m.services["db"].running || !strings.Contains(m.statusChip(m.services["db"], 9), "exit") {
		t.Fatalf("unready compose service rendered healthy: running=%v chip=%q", m.services["db"].running, m.statusChip(m.services["db"], 9))
	}
}

func TestComposeCommandUsesResolvedFilePathAndCancellation(t *testing.T) {
	tempDir := t.TempDir()
	workDir := filepath.Join(tempDir, "work")
	composeDir := filepath.Join(tempDir, "compose")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(composeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(tempDir, "args")
	stub := filepath.Join(tempDir, "compose-stub")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > '" + argsPath + "'\nprintf '%s\\n' '[{\"Service\":\"db\",\"State\":\"running\",\"Status\":\"Up\"}]'\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := serviceConfig{
		WorkDir:        workDir,
		ComposeFile:    filepath.Join(composeDir, "stack.yml"),
		ComposeCommand: []string{stub},
		ComposeServices: []string{
			"db",
		},
	}
	if _, err := readComposeStatusesContext(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), cfg.ComposeFile) {
		t.Fatalf("compose file directory was discarded: args=%q", args)
	}

	sleepStub := filepath.Join(tempDir, "compose-sleep")
	if err := os.WriteFile(sleepStub, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.ComposeCommand = []string{sleepStub}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := readComposeStatusesContext(ctx, cfg); err == nil {
		t.Fatal("expected compose cancellation")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("compose cancellation took %s", elapsed)
	}
}

func TestSingletonLockRejectsSecondOwnerAndReleases(t *testing.T) {
	root := t.TempDir()
	release, err := acquireSingletonLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if secondRelease, err := acquireSingletonLock(root); err == nil {
		secondRelease()
		release()
		t.Fatal("second owner acquired singleton lock")
	}
	release()

	reacquired, err := acquireSingletonLock(root)
	if err != nil {
		t.Fatalf("released lock could not be reacquired: %v", err)
	}
	reacquired()
}

func TestSingletonLockHasOneConcurrentOwner(t *testing.T) {
	root := t.TempDir()
	start := make(chan struct{})
	results := make(chan error, 2)
	releases := make(chan func(), 2)
	for range 2 {
		go func() {
			<-start
			release, err := acquireSingletonLock(root)
			if err == nil {
				releases <- release
			}
			results <- err
		}()
	}
	close(start)

	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	close(releases)
	for release := range releases {
		release()
	}
	if successes != 1 {
		t.Fatalf("concurrent lock successes = %d, want 1", successes)
	}
}

func TestComposeProjectLockWaitIsCancellable(t *testing.T) {
	cfg := serviceConfig{
		WorkDir:        t.TempDir(),
		ComposeFile:    filepath.Join(t.TempDir(), "compose.yml"),
		ComposeCommand: []string{"compose-test"},
	}
	unlock, err := lockComposeProjectContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if secondUnlock, err := lockComposeProjectContext(ctx, cfg); err == nil {
		secondUnlock()
		t.Fatal("second compose operation acquired a locked project")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("compose lock error = %v, want deadline exceeded", err)
	}
}

func TestOperationTrackerRejectsCommandsAfterShutdown(t *testing.T) {
	tracker := newOperationTracker()
	ran := false
	command := tracker.command(func() tea.Msg {
		ran = true
		return tickMsg(time.Now())
	})
	if err := tracker.waitAndStopPending(); err != nil {
		t.Fatal(err)
	}
	if message := command(); message != nil {
		t.Fatalf("command returned %T after tracker shutdown", message)
	}
	if ran {
		t.Fatal("queued command ran after tracker shutdown")
	}
}

func TestStaleHealthResultDoesNotAffectReplacement(t *testing.T) {
	state := &serviceState{
		config:     serviceConfig{Key: "app", Name: "App"},
		running:    true,
		generation: 2,
	}
	m := testSupervisorModel(map[string]*serviceState{"app": state}, []string{"app"})
	updated, _ := m.Update(healthCheckMsg{results: []serviceHealthResult{{
		key:        "app",
		generation: 1,
		err:        errors.New("stale failure"),
	}}})
	got := updated.(model).services["app"]
	if !got.running || got.liveFailures != 0 || got.exitErr != nil {
		t.Fatalf("stale health result changed replacement: %+v", got)
	}
}

func TestFailedReplacementExitDoesNotLeakOrPoisonNextGeneration(t *testing.T) {
	state := &serviceState{
		config:     serviceConfig{Key: "app", Name: "App", AutoRestart: true},
		generation: 1,
		restarting: true,
	}
	m := testSupervisorModel(map[string]*serviceState{"app": state}, []string{"app"})

	updated, _ := m.Update(processExitMsg{key: "app", pid: 4242, generation: 2, err: errors.New("replacement exited")})
	m = updated.(model)
	if len(m.earlyExits) != 1 {
		t.Fatalf("early exits = %d, want 1", len(m.earlyExits))
	}
	updated, _ = m.Update(restartResultMsg{key: "app", generation: 2, oldStopped: true, err: errors.New("readiness failed")})
	m = updated.(model)
	if len(m.earlyExits) != 0 || state.generation != 2 {
		t.Fatalf("failed generation was not retired: generation=%d early=%v", state.generation, m.earlyExits)
	}

	state.restarting = true
	next := &serviceState{config: state.config, pid: 4242, running: true, generation: 3}
	m.applyRestartResult(restartResultMsg{key: "app", generation: 3, next: next, oldStopped: true})
	if !state.running || state.generation != 3 {
		t.Fatalf("next generation was poisoned: %+v", state)
	}
}

func TestProcessExitBeforeBootResultIsApplied(t *testing.T) {
	state := &serviceState{config: serviceConfig{Key: "app", Name: "App"}, starting: true}
	m := testSupervisorModel(map[string]*serviceState{"app": state}, []string{"app"})
	m.bootRemaining = 1
	m.bootResults = make(chan serviceBootResultMsg)

	updated, _ := m.Update(processExitMsg{key: "app", pid: 99, generation: 1, err: errors.New("exited")})
	m = updated.(model)
	updated, _ = m.Update(serviceBootResultMsg{
		key:   "app",
		state: &serviceState{config: state.config, pid: 99, running: true, generation: 1},
	})
	got := updated.(model).services["app"]
	if got.running || got.exitErr == nil {
		t.Fatalf("early startup exit was lost: %+v", got)
	}
}

func TestShutdownLayersStopConsumersBeforeProviders(t *testing.T) {
	process := func(pid int) *exec.Cmd { return &exec.Cmd{Process: &os.Process{Pid: pid}} }
	services := map[string]*serviceState{
		"db":     {config: serviceConfig{Key: "db"}, cmd: process(1)},
		"api":    {config: serviceConfig{Key: "api", DependsOn: []string{"db"}}, cmd: process(2)},
		"worker": {config: serviceConfig{Key: "worker", DependsOn: []string{"db"}}, cmd: process(3)},
	}
	layers := shutdownServiceLayers(services, []string{"db", "api", "worker"})
	if len(layers) != 2 || strings.Join(layers[0], ",") != "worker,api" || strings.Join(layers[1], ",") != "db" {
		t.Fatalf("shutdown layers = %v", layers)
	}
}

func TestQuitKeepsTUIAliveUntilShutdownProgressCompletes(t *testing.T) {
	compose := &serviceState{
		config:  serviceConfig{Key: "db", Name: "DB", ComposeServices: []string{"db"}},
		running: true,
	}
	app := &serviceState{config: serviceConfig{Key: "app", Name: "App"}}
	m := testSupervisorModel(map[string]*serviceState{"db": compose, "app": app}, []string{"db", "app"})
	bootDone := make(chan struct{})
	close(bootDone)
	m.bootDone = bootDone

	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = updated.(model)
	if !m.shuttingDown || command == nil {
		t.Fatalf("quit did not enter visible shutdown: shuttingDown=%v command=%v", m.shuttingDown, command != nil)
	}
	ready, ok := command().(shutdownReadyMsg)
	if !ok {
		t.Fatal("quit command exited Bubble Tea instead of preparing shutdown")
	}
	updated, command = m.Update(ready)
	m = updated.(model)
	if !m.shutdownStart || command == nil {
		t.Fatal("service shutdown did not start inside the TUI")
	}

	for {
		progress, ok := command().(shutdownProgressMsg)
		if !ok {
			t.Fatal("shutdown progress command returned an unexpected message")
		}
		updated, command = m.Update(progress)
		m = updated.(model)
		if progress.done {
			break
		}
		if command == nil {
			t.Fatal("TUI stopped waiting before shutdown completed")
		}
	}

	if !compose.keptRunning || !compose.running {
		t.Fatalf("compose service was not shown as retained: %+v", compose)
	}
	if !app.shutdownDone || app.running {
		t.Fatalf("inactive process service was not shown as stopped: %+v", app)
	}
	if m.shutdownPhase != "shutdown complete" || command == nil {
		t.Fatalf("final shutdown frame was not retained: phase=%q command=%v", m.shutdownPhase, command != nil)
	}
}

func TestShutdownProgressReportsStoppingBeforeStopped(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exec sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-waitDone:
		default:
		}
	})

	services := map[string]*serviceState{
		"app": {
			config:  serviceConfig{Key: "app", Name: "App"},
			cmd:     cmd,
			pid:     cmd.Process.Pid,
			running: true,
		},
	}
	var phases []string
	var shutdownErr error
	for progress := range beginShutdownServices(services, []string{"app"}, nil) {
		if progress.done {
			shutdownErr = progress.err
			continue
		}
		phases = append(phases, progress.phase)
	}
	if shutdownErr != nil {
		t.Fatal(shutdownErr)
	}
	if strings.Join(phases, ",") != "stopping,stopped" {
		t.Fatalf("shutdown phases = %v", phases)
	}
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("service survived completed shutdown")
	}
}

func testSupervisorModel(services map[string]*serviceState, order []string) model {
	ctx, cancel := context.WithCancel(context.Background())
	return model{
		services:    services,
		order:       order,
		exitCh:      make(chan processExitMsg, 16),
		runtimeCtx:  ctx,
		cancel:      cancel,
		operations:  newOperationTracker(),
		earlyExits:  make(map[uint64]error),
		maxLogBytes: 1024 * 1024,
	}
}
