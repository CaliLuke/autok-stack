package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestBootServicesRunsIndependentReadinessChecksConcurrently(t *testing.T) {
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

	tempDir := t.TempDir()
	configs := []serviceConfig{
		testBootService(tempDir, "one", server.URL),
		testBootService(tempDir, "two", server.URL),
	}
	exitCh := make(chan processExitMsg, 8)
	services, order, failed := bootServices(context.Background(), configs, exitCh, 1024*1024)
	t.Cleanup(func() { _ = shutdownServices(services, order) })

	if failed {
		t.Fatal("independent services unexpectedly failed")
	}
	if peak.Load() < 2 {
		t.Fatalf("readiness checks were serialized: peak concurrency=%d", peak.Load())
	}
}

func TestBootServicesSkipsFailedDependentsButStartsIndependentServices(t *testing.T) {
	tempDir := t.TempDir()
	configs := []serviceConfig{
		{
			Key:     "broken",
			Name:    "Broken",
			WorkDir: tempDir,
			LogFile: filepath.Join(tempDir, "broken.log"),
			Command: []string{filepath.Join(tempDir, "does-not-exist")},
		},
		{
			Key:       "dependent",
			Name:      "Dependent",
			DependsOn: []string{"broken"},
			WorkDir:   tempDir,
			LogFile:   filepath.Join(tempDir, "dependent.log"),
			Command:   []string{"sh", "-c", "sleep 5"},
		},
		testBootService(tempDir, "independent", ""),
	}
	exitCh := make(chan processExitMsg, 8)
	services, order, failed := bootServices(context.Background(), configs, exitCh, 1024*1024)
	t.Cleanup(func() { _ = shutdownServices(services, order) })

	if !failed {
		t.Fatal("expected boot failure")
	}
	if services["dependent"].running || services["dependent"].exitErr == nil {
		t.Fatalf("dependent service was not skipped: %+v", services["dependent"])
	}
	if !services["independent"].running {
		t.Fatalf("independent service did not start: %v", services["independent"].exitErr)
	}
}

func TestBootServicesCancellationStopsPartialLaunches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	tempDir := t.TempDir()
	pidPath := filepath.Join(tempDir, "pid")
	config := serviceConfig{
		Key:              "slow",
		Name:             "Slow",
		WorkDir:          tempDir,
		LogFile:          filepath.Join(tempDir, "slow.log"),
		Command:          []string{"sh", "-c", `echo $$ > "$1"; sleep 5`, "sh", pidPath},
		ReadyURL:         server.URL,
		ReadinessTimeout: 5 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	services, _, failed := bootServices(ctx, []serviceConfig{config}, make(chan processExitMsg, 2), 1024*1024)
	if !failed || services["slow"].running {
		t.Fatalf("cancelled boot remained healthy: %+v", services["slow"])
	}

	rawPID, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cancelled boot leaked pid %d", pid)
}

func TestBeginBootServicesReturnsBeforeReadiness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	services, order, results, done := beginBootServices(ctx, nil, []serviceConfig{
		testBootService(t.TempDir(), "slow", server.URL),
	}, make(chan processExitMsg, 2), 1024*1024)
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("beginBootServices blocked for %s", elapsed)
	}
	if !services["slow"].starting {
		t.Fatal("service was not exposed as starting")
	}
	cancel()
	for result := range results {
		services[result.key] = result.state
	}
	<-done
	if err := shutdownServices(services, order); err != nil {
		t.Fatal(err)
	}
}

func TestDependencyOrderPlacesProvidersBeforeConsumers(t *testing.T) {
	configs := []serviceConfig{
		{Key: "app", DependsOn: []string{"db"}},
		{Key: "db"},
		{Key: "worker", DependsOn: []string{"app"}},
	}
	ordered := dependencyOrderedConfigs(configs)
	got := []string{ordered[0].Key, ordered[1].Key, ordered[2].Key}
	want := []string{"db", "app", "worker"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dependency order = %v, want %v", got, want)
	}
}

func TestBootGroupsComposeServicesByProject(t *testing.T) {
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
	postgres := base
	postgres.Key = "postgres"
	postgres.ComposeServices = []string{"postgres"}
	typedb := base
	typedb.Key = "typedb"
	typedb.ComposeServices = []string{"typedb"}

	_, _, failed := bootServices(context.Background(), []serviceConfig{postgres, typedb}, make(chan processExitMsg, 2), 1024*1024)
	if failed {
		t.Fatal("grouped compose boot failed")
	}
	calls, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "x" {
		t.Fatalf("compose calls = %q, want one", calls)
	}
}

func testBootService(tempDir, key, readyURL string) serviceConfig {
	return serviceConfig{
		Key:              key,
		Name:             key,
		WorkDir:          tempDir,
		LogFile:          filepath.Join(tempDir, key+".log"),
		Command:          []string{"sh", "-c", "sleep 5"},
		ReadyURL:         readyURL,
		ReadinessTimeout: time.Second,
	}
}
