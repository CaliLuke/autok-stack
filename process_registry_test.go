package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRegistryRecoversTokenVerifiedGroupAfterSupervisorLoss(t *testing.T) {
	registryPath := filepath.Join(t.TempDir(), "processes.json")
	registry, err := openProcessRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	token, err := newProcessOwnerToken()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "while :; do sleep 1; done", serviceWrapperArg, token)
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
	if err := registry.register(processRecord{
		Key:       "app",
		PID:       cmd.Process.Pid,
		PGID:      cmd.Process.Pid,
		Token:     token,
		Command:   []string{"sh", "-c", "while :; do sleep 1; done"},
		WorkDir:   t.TempDir(),
		LogFile:   filepath.Join(t.TempDir(), "app.log"),
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Re-open from disk to model a fresh stack process after the supervisor was
	// killed without running any in-memory cleanup.
	recovered, err := openProcessRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	warnings, err := recovered.reapStale(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected recovery warnings: %v", warnings)
	}
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("owned stale process group survived recovery")
	}
	if records := recovered.snapshot(); len(records) != 0 {
		t.Fatalf("recovered registry still contains %v", records)
	}
}

func TestRegistryNeverKillsReusedProcessGroupWithoutToken(t *testing.T) {
	registry, err := openProcessRegistry(filepath.Join(t.TempDir(), "processes.json"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waitDone
	}()
	if err := registry.register(processRecord{
		Key:       "app",
		PID:       cmd.Process.Pid,
		PGID:      cmd.Process.Pid,
		Token:     "token-that-is-not-in-the-process-environment",
		Command:   []string{"sh", "-c", "sleep 30"},
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	warnings, err := registry.reapStale(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "ownership token did not match") {
		t.Fatalf("warnings = %v", warnings)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("unrelated process was killed: %v", err)
	}
	if records := registry.snapshot(); len(records) != 0 {
		t.Fatalf("mismatched stale record was retained: %v", records)
	}
}

func TestPortConflictDoesNotTerminateUnrelatedListener(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof is unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	err = ensurePortsAvailable(context.Background(), serviceConfig{Key: "app", Ports: []string{port}})
	if err == nil || !strings.Contains(err.Error(), "refusing to terminate unrelated port owner") {
		t.Fatalf("port conflict error = %v", err)
	}
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("unrelated listener was disrupted: %v", err)
	}
	_ = connection.Close()
}

func TestPortConflictReclaimsOnlyTokenVerifiedStackGroup(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof is unavailable")
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(probe.Addr().(*net.TCPAddr).Port)
	_ = probe.Close()

	token, err := newProcessOwnerToken()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `"$2" -m http.server "$3" --bind 127.0.0.1 & wait`, serviceWrapperArg, token, python, port)
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
		case <-time.After(time.Second):
		}
	})

	registry, err := openProcessRegistry(filepath.Join(t.TempDir(), "processes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.register(processRecord{
		Key:       "app",
		PID:       cmd.Process.Pid,
		PGID:      cmd.Process.Pid,
		Token:     token,
		Command:   []string{python, "-m", "http.server", port},
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	waitForListener(t, "127.0.0.1:"+port)

	if err := ensurePortsAvailable(context.Background(), serviceConfig{Key: "app", Ports: []string{port}, registry: registry}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("verified stack-owned listener survived reclamation")
	}
	if records := registry.snapshot(); len(records) != 0 {
		t.Fatalf("reclaimed registry still contains %v", records)
	}
}

func TestPrepareLogsPreservesOnePreviousSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	previous := path + ".previous"
	if err := os.WriteFile(path, []byte("first session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := []serviceConfig{{Key: "app", LogFile: path}}
	if err := prepareLogs(config); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, previous, "first session\n")
	assertFileContents(t, path, "")

	// An immediate retry with no new output must not erase the useful previous
	// session merely because startup failed before the service wrote a log.
	if err := prepareLogs(config); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, previous, "first session\n")

	if err := os.WriteFile(path, []byte("second session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareLogs(config); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, previous, "second session\n")
	assertFileContents(t, path, "")
}

func TestCorruptRegistryFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "processes.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := openProcessRegistry(path)
	if err == nil || !strings.Contains(err.Error(), "confirming no stack-owned processes remain") {
		t.Fatalf("corrupt registry error = %v", err)
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("%s = %q, want %q", path, raw, want)
	}
}

func waitForListener(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("listener %s did not start", address)
}

func TestPrepareLogsReportsUnwritablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "app.log")
	err := prepareLogs([]serviceConfig{{Key: "app", LogFile: path}})
	if err == nil || !strings.Contains(err.Error(), "create app log") {
		t.Fatalf("prepareLogs error = %v", err)
	}
}
