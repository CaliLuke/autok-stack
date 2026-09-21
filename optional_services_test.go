package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestAutostartConfigDefaultsAndOptOut(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		manual        bool
	}{
		{"omitted", "", false}, {"enabled", "autostart = true", false}, {"optional", "autostart = false", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeStackConfig(t, dir, "[[service]]\nkey = \"app\"\ncommand = [\"sleep\", \"5\"]\nauto_restart = true\n"+tc.setting)
			cfg, err := loadConfig(dir)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.services[0].ManualStart != tc.manual || !cfg.services[0].AutoRestart {
				t.Fatalf("unexpected startup policy: %+v", cfg.services[0])
			}
		})
	}
}

func bootOptionalModel(t *testing.T, configs ...serviceConfig) model {
	t.Helper()
	exits := make(chan processExitMsg, 32)
	services, order, failed := bootServices(context.Background(), configs, exits, 1024*1024)
	m := testSupervisorModel(services, order)
	m.exitCh = exits
	m.anyExited = failed
	m.styles = newStyles()
	m.width, m.height = 100, 28
	t.Cleanup(func() {
		m.cancel()
		if err := m.operations.waitAndStopPending(); err != nil {
			t.Error(err)
		}
		if err := shutdownServices(services, order); err != nil {
			t.Error(err)
		}
	})
	return m
}

func optionalConfig(t *testing.T, key string) serviceConfig {
	t.Helper()
	cfg := testBootService(t.TempDir(), key, "")
	cfg.ManualStart = true
	cfg.Command = []string{"sh", "-c", "sleep 60"}
	return cfg
}

// Drive only commands produced by Start; no perpetual heartbeat commands.
func runStartCommands(t *testing.T, m *model, command tea.Cmd) {
	t.Helper()
	if command == nil {
		return
	}
	message := command()
	if batch, ok := message.(tea.BatchMsg); ok {
		for _, child := range batch {
			runStartCommands(t, m, child)
		}
		return
	}
	updated, next := m.Update(message)
	*m = updated.(model)
	runStartCommands(t, m, next)
}

func pressStart(t *testing.T, m *model, key string) tea.Cmd {
	t.Helper()
	for i, candidate := range m.order {
		if candidate == key {
			m.selected = i
			updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
			*m = updated.(model)
			return cmd
		}
	}
	t.Fatalf("service %s not found", key)
	return nil
}

func TestOptionalServicesStayInactiveAndHealthyAcrossLaunches(t *testing.T) {
	app := optionalConfig(t, "app")
	app.ManualStart = false
	storybook := optionalConfig(t, "storybook")
	storybook.AutoRestart = true
	design := optionalConfig(t, "design")
	m := bootOptionalModel(t, app, storybook, design)
	updated, _ := m.Update(tickMsg(time.Now()))
	m = updated.(model)
	if !m.services["app"].running || !m.services["storybook"].inactive || !m.services["design"].inactive || m.anyExited || m.anyAnimating() {
		t.Fatalf("unexpected launch state: %+v", m.services)
	}
	if _, err := os.Stat(storybook.LogFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inactive service wrote logs: %v", err)
	}
	if !strings.Contains(stripANSI(m.renderHero(100)), "1/1 live") {
		t.Fatal("inactive services lowered the live count")
	}
	runStartCommands(t, &m, pressStart(t, &m, "storybook"))
	if !m.services["storybook"].running {
		t.Fatal("requested service did not start")
	}
	if err := shutdownServices(m.services, m.order); err != nil {
		t.Fatal(err)
	}
	next := bootOptionalModel(t, storybook, design)
	if !next.services["storybook"].inactive || next.anyExited {
		t.Fatal("new launch retained prior activation")
	}
}

func TestAutomaticServiceStartsTransitiveOptionalDependencies(t *testing.T) {
	db := optionalConfig(t, "db")
	api := optionalConfig(t, "api")
	api.DependsOn = []string{"db"}
	app := optionalConfig(t, "app")
	app.ManualStart = false
	app.DependsOn = []string{"api"}
	other := optionalConfig(t, "other")
	m := bootOptionalModel(t, app, other, api, db)
	if !m.services["db"].running || !m.services["api"].running || !m.services["app"].running || !m.services["other"].inactive {
		t.Fatal("incorrect automatic dependency closure")
	}
}

func TestStartUsesDependenciesAssignedPortsLogsAndHealth(t *testing.T) {
	_, port := occupiedTestPort(t)
	provider := portTestConfig(t, "provider", port)
	provider.ManualStart = true
	storybook := optionalConfig(t, "storybook")
	storybook.DependsOn = []string{"provider"}
	storybook.Env = map[string]string{"PROVIDER": "http://localhost:{{port:provider:" + port + "}}"}
	other := optionalConfig(t, "other")
	m := bootOptionalModel(t, storybook, other, provider)
	command := pressStart(t, &m, "storybook")
	if !m.services["storybook"].startPending || !m.services["provider"].starting {
		t.Fatal("consumer did not wait for its provider")
	}
	runStartCommands(t, &m, command)
	state := m.services["provider"]
	if !state.running || state.config.Ports[0] == port || !m.services["storybook"].running || !m.services["other"].inactive {
		t.Fatalf("unexpected start result: %+v", state)
	}
	if !strings.Contains(m.services["storybook"].config.Env["PROVIDER"], ":"+state.config.Ports[0]) {
		t.Fatal("dependent retained preferred port")
	}
	if !strings.Contains(readLogTail(state.config.LogFile, 18), "started pid=") {
		t.Fatal("missing process log")
	}
	health := healthCheckCmd(m.runtimeCtx, m.operations, m.services)().(healthCheckMsg)
	if len(health.results) != 2 {
		t.Fatalf("health checked %d services", len(health.results))
	}
	for _, result := range health.results {
		if result.err != nil {
			t.Fatal(result.err)
		}
	}
}

func TestStartFailureIsVisibleAndRetriesDependencies(t *testing.T) {
	var ready atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	dependency := optionalConfig(t, "dependency")
	dependency.ReadyURL, dependency.ReadinessTimeout = server.URL, 30*time.Millisecond
	storybook := optionalConfig(t, "storybook")
	storybook.DependsOn = []string{"dependency"}
	m := bootOptionalModel(t, storybook, dependency)
	runStartCommands(t, &m, pressStart(t, &m, "storybook"))
	if !m.anyExited || m.services["storybook"].running || m.services["dependency"].running {
		t.Fatal("failed readiness was treated as healthy")
	}
	if err := m.services["storybook"].exitErr; err == nil || !strings.Contains(err.Error(), "dependency") {
		t.Fatalf("dependency failure missing: %v", err)
	}
	if !strings.Contains(stripANSI(m.renderFooter(100)), "s retry start") {
		t.Fatal("retry action is not discoverable")
	}
	ready.Store(true)
	runStartCommands(t, &m, pressStart(t, &m, "storybook"))
	if m.anyExited || !m.services["storybook"].running || !m.services["dependency"].running {
		t.Fatal("retry did not recover both services")
	}
}

func TestOptionalServiceRestartsOnlyAfterRequest(t *testing.T) {
	cfg := optionalConfig(t, "storybook")
	cfg.AutoRestart = true
	m := bootOptionalModel(t, cfg)
	updated, _ := m.Update(tickMsg(time.Now()))
	m = updated.(model)
	if m.services[cfg.Key].restarting {
		t.Fatal("auto_restart activated inactive service")
	}
	runStartCommands(t, &m, pressStart(t, &m, cfg.Key))
	old := m.services[cfg.Key].generation
	if err := m.services[cfg.Key].process.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-m.exitCh:
		updated, _ = m.Update(exit)
		m = updated.(model)
	case <-time.After(3 * time.Second):
		t.Fatal("no exit notification")
	}
	m.services[cfg.Key].nextRestartAt = time.Now().Add(-time.Second)
	m.healthBusy = true
	updated, command := m.Update(tickMsg(time.Now()))
	m = updated.(model)
	if !m.services[cfg.Key].restarting {
		t.Fatal("activated service did not schedule crash recovery")
	}
	// The first batch command is the next heartbeat; execute only recovery.
	batch := command().(tea.BatchMsg)
	runStartCommands(t, &m, batch[1])
	if !m.services[cfg.Key].running || m.services[cfg.Key].generation <= old {
		t.Fatal("crash recovery did not start a new generation")
	}
}

func TestConcurrentStartRequestsShareDependency(t *testing.T) {
	dependency := optionalConfig(t, "dependency")
	one, two := optionalConfig(t, "one"), optionalConfig(t, "two")
	one.DependsOn, two.DependsOn = []string{"dependency"}, []string{"dependency"}
	m := bootOptionalModel(t, one, two, dependency)
	command := pressStart(t, &m, "one")
	if duplicate := pressStart(t, &m, "one"); duplicate != nil {
		t.Fatal("duplicate request scheduled work")
	}
	if additional := pressStart(t, &m, "two"); additional != nil {
		t.Fatal("shared dependency was scheduled twice")
	}
	runStartCommands(t, &m, command)
	if !m.services["one"].running || !m.services["two"].running || m.services["dependency"].generation != 2 {
		t.Fatal("shared dependency start failed")
	}
}

func TestInactiveComposeSkipsHealthAndStartsWithUp(t *testing.T) {
	cfg := optionalConfig(t, "db")
	calls := filepath.Join(t.TempDir(), "calls")
	stub := filepath.Join(t.TempDir(), "compose")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\ncase \"$*\" in\n *restart*) exit 1;;\n *'up -d'*) touch '" + calls + ".up';;\n *ps*) if [ -f '" + calls + ".up' ]; then echo '[{\"Service\":\"db\",\"State\":\"running\"}]'; else echo '[]'; fi;;\nesac\n"
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Command = nil
	cfg.ComposeFile, cfg.ComposeServices, cfg.ComposeCommand = "compose.yml", []string{"db"}, []string{stub}
	m := bootOptionalModel(t, cfg)
	health := healthCheckCmd(m.runtimeCtx, m.operations, m.services)().(healthCheckMsg)
	if len(health.results) != 0 {
		t.Fatal("inactive compose service was probed")
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inactive compose service ran a command")
	}
	runStartCommands(t, &m, pressStart(t, &m, "db"))
	if !m.services["db"].running {
		t.Fatalf("compose start failed: %v", m.services["db"].exitErr)
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "up -d db") || strings.Contains(string(raw), "restart") {
		t.Fatalf("unexpected compose calls: %s", raw)
	}
}

func TestStartDuringBootWaitsForExistingDependency(t *testing.T) {
	dependency := optionalConfig(t, "dependency")
	dependency.ManualStart = false
	optional := optionalConfig(t, "optional")
	optional.DependsOn = []string{"dependency"}
	m := testSupervisorModel(map[string]*serviceState{
		"dependency": {config: dependency, starting: true},
		"optional":   {config: optional, inactive: true},
	}, []string{"dependency", "optional"})
	t.Cleanup(func() {
		m.cancel()
		_ = m.operations.waitAndStopPending()
		_ = shutdownServices(m.services, m.order)
	})
	m.bootRemaining = 2
	if command := pressStart(t, &m, "optional"); command != nil {
		t.Fatal("started before boot dependency was ready")
	}
	updated, _ := m.Update(serviceBootResultMsg{key: "optional", state: &serviceState{config: optional, inactive: true, generation: 1}})
	m = updated.(model)
	if !m.services["optional"].startPending || m.services["optional"].inactive {
		t.Fatal("delayed inactive boot result erased start request")
	}
	updated, command := m.Update(serviceBootResultMsg{key: "dependency", state: &serviceState{config: dependency, running: true, generation: 1}})
	m = updated.(model)
	runStartCommands(t, &m, command)
	if !m.services["optional"].running {
		t.Fatal("start did not resume after dependency boot")
	}
}

func TestShutdownCleansUpUnconsumedOptionalStart(t *testing.T) {
	cfg := optionalConfig(t, "optional")
	m := bootOptionalModel(t, cfg)
	command := pressStart(t, &m, cfg.Key)
	result := command().(restartResultMsg)
	if result.err != nil || result.next == nil {
		t.Fatalf("start failed: %v", result.err)
	}
	updated, shutdown := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = updated.(model)
	m.Update(result) // Shutdown discards results; the tracker still owns the child.
	ready := shutdown().(shutdownReadyMsg)
	if ready.pendingErr != nil {
		t.Fatal(ready.pendingErr)
	}
	if !result.next.process.stopped.Load() {
		t.Fatal("shutdown leaked the pending optional process")
	}
	if _, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}); command != nil {
		t.Fatal("shutdown allowed a new start")
	}
}

func TestDirectReadinessFailureCanBeRetriedWithStart(t *testing.T) {
	var ready atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	cfg := optionalConfig(t, "optional")
	cfg.ReadyURL, cfg.ReadinessTimeout = server.URL, 30*time.Millisecond
	m := bootOptionalModel(t, cfg)
	runStartCommands(t, &m, pressStart(t, &m, cfg.Key))
	if !m.anyExited || m.services[cfg.Key].exitErr == nil || m.services[cfg.Key].running {
		t.Fatal("readiness failure was hidden")
	}
	ready.Store(true)
	runStartCommands(t, &m, pressStart(t, &m, cfg.Key))
	if m.anyExited || !m.services[cfg.Key].running {
		t.Fatal("readiness retry failed")
	}
}

func TestOptionalComposeIsExcludedFromAutomaticComposeGroup(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	stub := filepath.Join(t.TempDir(), "compose")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\necho '[{\"Service\":\"db\",\"State\":\"running\"}]'\n"
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	db := serviceConfig{Key: "db", ComposeServices: []string{"db"}, ComposeFile: "compose.yml", ComposeCommand: []string{stub}}
	optional := db
	optional.Key, optional.ComposeServices, optional.ManualStart = "optional", []string{"optional"}, true
	m := bootOptionalModel(t, db, optional)
	if !m.services["db"].running || !m.services["optional"].inactive {
		t.Fatal("optional compose service joined automatic startup")
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "optional") {
		t.Fatalf("optional service was included in compose command: %s", raw)
	}
}
