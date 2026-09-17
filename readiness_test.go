package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForReadinessRequiresEveryDeclaredPort(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ports := []string{
		fmt.Sprint(first.Addr().(*net.TCPAddr).Port),
		fmt.Sprint(second.Addr().(*net.TCPAddr).Port),
	}
	if err := waitForReadiness(serviceConfig{Ports: ports, ReadinessTimeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitForReadiness(serviceConfig{Ports: ports, ReadinessTimeout: 50 * time.Millisecond}); err == nil {
		t.Fatal("expected readiness to fail when one declared port is closed")
	}
}

func TestWaitForReadinessUsesReadyURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := serviceConfig{ReadyURL: server.URL, ReadinessTimeout: time.Second}
	if err := waitForReadiness(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForReadinessRejectsUnhealthyURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := serviceConfig{ReadyURL: server.URL, ReadinessTimeout: 50 * time.Millisecond}
	if err := waitForReadiness(cfg); err == nil {
		t.Fatal("expected readiness failure")
	}
}

func TestCheckHealthURLRequiresOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "degraded", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	if err := checkHealthURL(server.URL); err == nil {
		t.Fatal("expected liveness failure")
	}
}

func TestBuildServiceConfigRejectsMalformedHealthURL(t *testing.T) {
	_, err := buildServiceConfig(t.TempDir(), t.TempDir(), serviceBlock{
		Key:      "logal",
		Command:  []string{"logal"},
		ReadyURL: "localhost:13133/readyz",
	})
	if err == nil {
		t.Fatal("expected malformed ready_url rejection")
	}
}

func TestResetRestartBackoffAfterHealthyWindow(t *testing.T) {
	now := time.Now()
	state := &serviceState{
		running:        true,
		restartAttempt: 5,
		startedAt:      now.Add(-31 * time.Second),
		nextRestartAt:  now.Add(time.Minute),
	}
	resetRestartBackoffAfterHealthy(state, now)
	if state.restartAttempt != 0 || !state.nextRestartAt.IsZero() {
		t.Fatalf("backoff did not reset: attempt=%d next=%v", state.restartAttempt, state.nextRestartAt)
	}
}

func TestRestartCommandStopsReplacementThatNeverBecomesReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	tempDir := t.TempDir()
	cfg := serviceConfig{
		Key:              "slow",
		Name:             "Slow",
		WorkDir:          tempDir,
		LogFile:          filepath.Join(tempDir, "slow.log"),
		Command:          []string{"sh", "-c", "sleep 5"},
		ReadyURL:         server.URL,
		ReadinessTimeout: 50 * time.Millisecond,
	}
	tracker := newOperationTracker()
	message := restartServiceCmd(context.Background(), tracker, cfg.Key, cfg, nil, 1, 1024*1024, make(chan processExitMsg, 2))().(restartResultMsg)
	if message.err == nil {
		t.Fatal("expected readiness failure")
	}
	if message.next != nil {
		t.Fatalf("failed replacement remained tracked: pid=%d", message.next.pid)
	}
}

func TestRestartResultResetsLivenessFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	tempDir := t.TempDir()
	state := &serviceState{
		config: serviceConfig{
			Key:              "recovering",
			Name:             "Recovering",
			WorkDir:          tempDir,
			LogFile:          filepath.Join(tempDir, "recovering.log"),
			Command:          []string{"sh", "-c", "sleep 5"},
			ReadyURL:         server.URL,
			ReadinessTimeout: time.Second,
		},
		liveFailures: 3,
	}
	m := testSupervisorModel(map[string]*serviceState{"recovering": state}, []string{"recovering"})
	message := restartServiceCmd(m.runtimeCtx, m.operations, state.config.Key, state.config, nil, 1, m.maxLogBytes, m.exitCh)().(restartResultMsg)
	if message.err != nil {
		t.Fatal(message.err)
	}
	m.applyRestartResult(message)
	t.Cleanup(func() { _ = shutdownServices(m.services, m.order) })
	if state.liveFailures != 0 || !state.running {
		t.Fatalf("restart did not recover health: running=%v failures=%d", state.running, state.liveFailures)
	}
}

func TestComposeReadinessFailureMarksServiceUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	tempDir := t.TempDir()
	composeStub := filepath.Join(tempDir, "compose-stub")
	if err := os.WriteFile(composeStub, []byte("#!/bin/sh\nprintf '%s\\n' '[{\"Service\":\"db\",\"State\":\"running\",\"Status\":\"Up\"}]'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := &serviceState{running: true, config: serviceConfig{
		Key:            "db",
		Name:           "DB",
		WorkDir:        tempDir,
		ComposeFile:    filepath.Join(tempDir, "compose.yml"),
		ComposeCommand: []string{composeStub},
		ComposeServices: []string{
			"db",
		},
		ReadyURL: server.URL,
	}}
	m := testSupervisorModel(map[string]*serviceState{"db": state}, []string{"db"})
	message := healthCheckCmd(m.runtimeCtx, m.operations, m.services)().(healthCheckMsg)
	updated, _ := m.Update(message)
	state = updated.(model).services["db"]
	if state.exitErr == nil || state.running {
		t.Fatalf("unready compose state was not preserved: running=%v err=%v", state.running, state.exitErr)
	}
}

func TestRefreshComposeServicesHealthBatchesSharedProject(t *testing.T) {
	tempDir := t.TempDir()
	countPath := filepath.Join(tempDir, "calls")
	composeStub := filepath.Join(tempDir, "compose-stub")
	script := "#!/bin/sh\nprintf x >> '" + countPath + "'\nprintf '%s\\n' '[{\"Service\":\"postgres\",\"State\":\"running\",\"Status\":\"Up\"},{\"Service\":\"typedb\",\"State\":\"running\",\"Status\":\"Up\"}]'\n"
	if err := os.WriteFile(composeStub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	base := serviceConfig{
		WorkDir:        tempDir,
		ComposeFile:    filepath.Join(tempDir, "compose.yml"),
		ComposeCommand: []string{composeStub},
	}
	postgres := &serviceState{config: base}
	postgres.config.Key = "postgres"
	postgres.config.Name = "Postgres"
	postgres.config.ComposeServices = []string{"postgres"}
	typedb := &serviceState{config: base}
	typedb.config.Key = "typedb"
	typedb.config.Name = "TypeDB"
	typedb.config.ComposeServices = []string{"typedb"}

	services := map[string]*serviceState{
		"postgres": postgres,
		"typedb":   typedb,
	}
	tracker := newOperationTracker()
	message := healthCheckCmd(context.Background(), tracker, services)().(healthCheckMsg)
	if len(message.results) != 2 {
		t.Fatalf("health results = %d, want 2", len(message.results))
	}
	calls, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "x" {
		t.Fatalf("compose status calls = %q, want one", calls)
	}
	for _, result := range message.results {
		if result.err != nil || !result.running {
			t.Fatalf("batched health result for %s: running=%v err=%v", result.key, result.running, result.err)
		}
	}
}

func TestSteadyStateHealthPrefersLiveURL(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer ready.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer live.Close()

	if err := checkServiceHealthOnce(context.Background(), serviceConfig{ReadyURL: ready.URL, LiveURL: live.URL}); err != nil {
		t.Fatalf("healthy liveness endpoint was ignored: %v", err)
	}
}

func TestHealthChecksRunIndependently(t *testing.T) {
	var active atomic.Int32
	var peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		defer active.Add(-1)
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	services := map[string]*serviceState{
		"one": {config: serviceConfig{Key: "one", LiveURL: server.URL}, running: true, generation: 1},
		"two": {config: serviceConfig{Key: "two", LiveURL: server.URL}, running: true, generation: 1},
	}
	tracker := newOperationTracker()
	message := healthCheckCmd(context.Background(), tracker, services)().(healthCheckMsg)
	if len(message.results) != 2 {
		t.Fatalf("health results = %d, want 2", len(message.results))
	}
	if peak.Load() < 2 {
		t.Fatalf("health checks were serialized: peak concurrency=%d", peak.Load())
	}
}

func TestHungHealthEndpointIsBounded(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	started := time.Now()
	err := checkServiceHealthOnce(context.Background(), serviceConfig{LiveURL: server.URL})
	if err == nil {
		t.Fatal("hung health endpoint unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("hung health check took %s", elapsed)
	}
}

func TestReadLogTailReadsFromEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	lines := make([]string, 2000)
	for i := range lines {
		lines[i] = fmt.Sprintf("%04d %s", i, strings.Repeat("x", 32))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	want := strings.Join(lines[len(lines)-3:], "\n")
	if got := readLogTail(path, 3); got != want {
		t.Fatalf("tail mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestCapLogFileUsesHysteresis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := capLogFile(path, 100); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 75 {
		t.Fatalf("trimmed size = %d, want 75", info.Size())
	}
}

func TestCappedLogWriterRetainsRecentOutputWithoutExceedingCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	writer, err := openCappedLogWriter(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{strings.Repeat("a", 60), strings.Repeat("b", 60), strings.Repeat("c", 20)} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) > 100 {
		t.Fatalf("capped log size = %d", len(content))
	}
	if !strings.HasSuffix(string(content), strings.Repeat("c", 20)) {
		t.Fatalf("recent output was not retained: %q", content)
	}
}
