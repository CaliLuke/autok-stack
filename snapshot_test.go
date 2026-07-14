package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestUISnapshot renders the TUI at representative sizes/states with the lipgloss
// color profile forced to Ascii, then writes each snapshot to .tmp/snapshots/
// so we can read the rendered UI without a real terminal.
//
// Run with: go test -run TestUISnapshot -v
func TestUISnapshot(t *testing.T) {
	// Force ASCII renderer so output is human-readable when we cat the files.
	lipgloss.SetDefaultRenderer(lipgloss.NewRenderer(os.Stderr, termenv.WithProfile(termenv.Ascii)))

	cases := []struct {
		name   string
		width  int
		height int
		mutate func(*model)
	}{
		{name: "wide-healthy", width: 140, height: 40},
		{name: "medium-healthy", width: 100, height: 28},
		{name: "narrow-healthy", width: 70, height: 20},
		{name: "compact-fallback", width: 50, height: 10},
		{name: "wide-mixed-states", width: 140, height: 40, mutate: func(m *model) {
			m.services["logal"].running = false
			m.services["logal"].starting = true
			m.services["server"].running = false
			m.services["server"].exitErr = fmt.Errorf("dial tcp 127.0.0.1:5432: connect: connection refused")
			m.services["server"].stoppedAt = time.Now()
			m.services["frontend"].restarting = true
			m.services["admin"].running = false
			m.services["admin"].composeDown = true
			m.anyExited = true
		}},
		{name: "wide-spinner-frame-5", width: 140, height: 40, mutate: func(m *model) {
			m.services["frontend"].restarting = true
			m.services["server"].restarting = true
			m.spinnerFrame = 5
		}},
		{name: "wide-degraded-flash-on", width: 140, height: 40, mutate: func(m *model) {
			m.services["server"].running = false
			m.services["server"].exitErr = fmt.Errorf("uvicorn exited with code 1")
			m.anyExited = true
			m.spinnerFrame = 0 // flash "on" half of the cycle
		}},
		{name: "wide-degraded-flash-off", width: 140, height: 40, mutate: func(m *model) {
			m.services["server"].running = false
			m.services["server"].exitErr = fmt.Errorf("uvicorn exited with code 1")
			m.anyExited = true
			m.spinnerFrame = 5 // flash "off" half of the cycle
		}},
		{name: "wide-selected-server", width: 140, height: 40, mutate: func(m *model) {
			m.selected = 3 // server
		}},
		{name: "wide-shutting-down", width: 140, height: 40, mutate: func(m *model) {
			m.shuttingDown = true
			m.shutdownPhase = "stopping services"
			m.services["postgres"].keptRunning = true
			m.services["typedb"].keptRunning = true
			m.services["logal"].running = false
			m.services["logal"].pid = 0
			m.services["logal"].shutdownDone = true
			m.services["server"].stopping = true
			m.services["frontend"].stopping = true
			m.selected = 3
		}},
		{name: "compact-shutting-down", width: 50, height: 10, mutate: func(m *model) {
			m.shuttingDown = true
			m.shutdownPhase = "stopping services"
			m.services["postgres"].keptRunning = true
			m.services["server"].stopping = true
		}},
		{name: "narrow-shutting-down", width: 70, height: 20, mutate: func(m *model) {
			m.shuttingDown = true
			m.shutdownPhase = "draining in-flight work"
			m.services["postgres"].keptRunning = true
			m.services["typedb"].keptRunning = true
			m.services["server"].stopping = true
			m.selected = 3
		}},
	}

	outDir := ".tmp/snapshots"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range cases {
		m := newFakeModel()
		m.width = tc.width
		m.height = tc.height
		if tc.mutate != nil {
			tc.mutate(&m)
		}

		rendered := stripANSI(m.View())
		path := fmt.Sprintf("%s/%s.txt", outDir, tc.name)
		header := fmt.Sprintf("=== %s  (%dx%d) ===\n", tc.name, tc.width, tc.height)
		if err := os.WriteFile(path, []byte(header+rendered+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
	}
}

func newFakeModel() model {
	now := time.Now()
	services := map[string]*serviceState{}
	order := []string{"postgres", "typedb", "logal", "server", "frontend", "auth", "admin"}

	specs := []struct {
		key, name string
		ports     []string
		isCompose bool
		uptimeS   int
		pid       int
		log       string
	}{
		{"postgres", "Postgres", []string{"8002", "6432"}, true, 3600, 0, "postgres   running Up 1 hour\npgbouncer  running Up 1 hour"},
		{"typedb", "TypeDB", []string{"1729", "8003"}, true, 3600, 0, "typedb     running Up 1 hour health=healthy"},
		{"logal", "Logal", []string{"4318", "3847", "3848"}, false, 180, 12345, "[2026-05-15 16:42:01] INFO logal listener bound\n[2026-05-15 16:42:01] INFO buffer ready"},
		{"server", "Server", []string{"8000"}, false, 95, 12346, "INFO     Uvicorn running on http://0.0.0.0:8000\nINFO     Started reloader process [12346] using StatReload"},
		{"frontend", "Frontend", []string{"3000"}, false, 60, 12347, "  VITE v5.4.10  ready in 312 ms\n  ➜  Local:   http://localhost:3000/\n  ➜  Network: use --host to expose"},
		{"auth", "Auth", []string{"3001"}, false, 45, 12348, "[auth] listening on :3001\n[auth] better-auth ready"},
		{"admin", "Admin", []string{"5173"}, false, 30, 12349, "  VITE v5.4.10  ready in 280 ms\n  ➜  Local:   http://localhost:5173/"},
	}

	for _, s := range specs {
		cfg := serviceConfig{
			Key:     s.key,
			Name:    s.name,
			Ports:   s.ports,
			WorkDir: "/Users/luca/code/autok/" + s.key,
			LogFile: "/Users/luca/code/autok/.tmp/dev-stack/" + s.key + ".log",
			Command: []string{"bun", "run", "dev"},
		}
		if s.isCompose {
			cfg.ComposeFile = "/Users/luca/code/autok/autok-server/docker-compose.yml"
			cfg.ComposeServices = []string{s.key}
		}
		services[s.key] = &serviceState{
			config:      cfg,
			pid:         s.pid,
			running:     true,
			startedAt:   now.Add(-time.Duration(s.uptimeS) * time.Second),
			lastLogTail: s.log,
		}
	}

	return model{
		rootDir:     "/Users/luca/code/autok",
		logDir:      "/Users/luca/code/autok/.tmp/dev-stack",
		title:       "AUTO-K STACK",
		maxLogBytes: 1048576,
		services:    services,
		order:       order,
		styles:      newStyles(),
	}
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string {
	clean := ansiRE.ReplaceAllString(s, "")
	// Remove trailing spaces per line so snapshots aren't ragged when re-viewed.
	lines := strings.Split(clean, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}
