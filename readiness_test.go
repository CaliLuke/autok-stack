package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestAutoRestartStopsReplacementThatNeverBecomesReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	tempDir := t.TempDir()
	state := &serviceState{
		config: serviceConfig{
			Key:              "slow",
			Name:             "Slow",
			WorkDir:          tempDir,
			LogFile:          tempDir + "/slow.log",
			Command:          []string{"sh", "-c", "sleep 5"},
			ReadyURL:         server.URL,
			ReadinessTimeout: 50 * time.Millisecond,
		},
		ignoredExits: make(map[int]struct{}),
	}
	m := model{exitCh: make(chan processExitMsg, 2)}
	if err := m.autoRestartService(state); err == nil {
		t.Fatal("expected readiness failure")
	}
	if state.running || state.pid != 0 || state.cmd != nil {
		t.Fatalf("failed replacement remained active: running=%v pid=%d", state.running, state.pid)
	}
}

func TestAutoRestartResetsLivenessFailures(t *testing.T) {
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
		ignoredExits: make(map[int]struct{}),
	}
	m := model{exitCh: make(chan processExitMsg, 2)}
	if err := m.autoRestartService(state); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopProcessGroup(state.pid) })
	if state.liveFailures != 0 {
		t.Fatalf("liveness failures were not reset: %d", state.liveFailures)
	}
}

func TestManualRestartClearsRestartingAfterReadinessFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	tempDir := t.TempDir()
	state := &serviceState{
		config: serviceConfig{
			Key:              "slow",
			Name:             "Slow",
			WorkDir:          tempDir,
			LogFile:          filepath.Join(tempDir, "slow.log"),
			Command:          []string{"sh", "-c", "sleep 5"},
			ReadyURL:         server.URL,
			ReadinessTimeout: 50 * time.Millisecond,
		},
		ignoredExits: make(map[int]struct{}),
	}
	m := model{
		services: map[string]*serviceState{"slow": state},
		order:    []string{"slow"},
		exitCh:   make(chan processExitMsg, 2),
	}
	if err := m.restartSelectedService(); err == nil {
		t.Fatal("expected readiness failure")
	}
	if state.restarting {
		t.Fatal("failed manual restart remained marked as restarting")
	}
	if state.running || state.pid != 0 || state.cmd != nil {
		t.Fatalf("failed replacement remained active: running=%v pid=%d", state.running, state.pid)
	}
}

func TestComposeReadinessFailureSurvivesStatusRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	tempDir := t.TempDir()
	composeStub := filepath.Join(tempDir, "compose-stub")
	if err := os.WriteFile(composeStub, []byte("#!/bin/sh\nprintf '%s\\n' '[{\"Service\":\"db\",\"State\":\"running\",\"Status\":\"Up\"}]'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := &serviceState{config: serviceConfig{
		WorkDir:        tempDir,
		ComposeFile:    filepath.Join(tempDir, "compose.yml"),
		ComposeCommand: []string{composeStub},
		ComposeServices: []string{
			"db",
		},
		ReadyURL: server.URL,
	}}
	for range 2 {
		if err := refreshComposeServiceHealth(state); err == nil {
			t.Fatal("expected compose readiness failure")
		}
		if state.exitErr == nil || !state.running {
			t.Fatalf("unready compose state was cleared: running=%v err=%v", state.running, state.exitErr)
		}
	}
}
