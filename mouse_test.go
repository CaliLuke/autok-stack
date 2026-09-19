package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func mouseTestModel(t *testing.T, width, height int) model {
	t.Helper()
	m := newFakeModel()
	m.width, m.height = width, height
	for key, state := range m.services {
		state.config.LogFile = filepath.Join(t.TempDir(), key+".log")
		if err := os.WriteFile(state.config.LogFile, []byte("fresh log for "+key), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// Match terminal coordinates against the visible render, not layout constants.
func visibleServiceCell(t *testing.T, m model, key string) (int, int, bool) {
	t.Helper()
	lines := strings.Split(stripANSI(m.View()), "\n")
	if len(lines) > m.height {
		lines = lines[len(lines)-m.height:]
	}
	state := m.services[key]
	for y, line := range lines {
		if pos := strings.Index(line, state.config.Name); pos >= 0 && strings.Contains(line, strings.Join(state.config.Ports, ",")) {
			return lipgloss.Width(line[:pos]), y, true
		}
	}
	return 0, 0, false
}

func TestMouseClickSelectsVisibleServiceAndLogs(t *testing.T) {
	for _, size := range [][2]int{{140, 40}, {100, 28}, {70, 20}, {60, 12}, {50, 30}, {50, 10}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m := mouseTestModel(t, size[0], size[1])
			clicked := 0
			for i, key := range m.order {
				x, y, visible := visibleServiceCell(t, m, key)
				if !visible {
					continue
				}
				updated, _ := m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
				m = updated.(model)
				if m.selected != i {
					t.Fatalf("click on %s at (%d,%d) selected %d, want %d", key, x, y, m.selected, i)
				}
				if !m.services[key].config.isComposeService() && !strings.Contains(m.View(), "fresh log for "+key) {
					t.Fatalf("click did not refresh and display %s logs", key)
				}
				clicked++
			}
			if clicked == 0 {
				t.Fatal("no visible services exercised")
			}
		})
	}
}

func TestMouseIgnoresOtherPanelsAndNonClicks(t *testing.T) {
	m := mouseTestModel(t, 140, 40)
	x, y, _ := visibleServiceCell(t, m, "frontend")
	for _, msg := range []tea.MouseMsg{
		{X: x, Y: y, Button: tea.MouseButtonRight},
		{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease},
		{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion},
		{X: 0, Y: y, Button: tea.MouseButtonLeft},
		{X: m.width - 1, Y: y, Button: tea.MouseButtonLeft},
		{X: x, Y: 0, Button: tea.MouseButtonLeft},
		{X: x, Y: 20, Button: tea.MouseButtonLeft},
		{X: x, Y: m.height - 1, Button: tea.MouseButtonLeft},
		{X: -1, Y: y, Button: tea.MouseButtonLeft},
		{X: x, Y: m.height, Button: tea.MouseButtonLeft},
	} {
		updated, _ := m.Update(msg)
		if got := updated.(model).selected; got != m.selected {
			t.Errorf("%+v selected %d", msg, got)
		}
	}
}

func TestMouseWheelStepsAndSuppressesBursts(t *testing.T) {
	m := mouseTestModel(t, 140, 40)
	m.selected = 2
	now := time.Now()
	steps := []struct {
		after  time.Duration
		button tea.MouseButton
		want   int
	}{
		{0, tea.MouseButtonWheelDown, 3},
		{time.Millisecond, tea.MouseButtonWheelDown, 3},
		{2 * time.Millisecond, tea.MouseButtonWheelDown, 3},
		{wheelStepInterval, tea.MouseButtonWheelDown, 4},
		{wheelStepInterval + time.Millisecond, tea.MouseButtonWheelUp, 3},
		{2 * wheelStepInterval, tea.MouseButtonWheelRight, 3},
		{3 * wheelStepInterval, tea.MouseButtonWheelUp, 2},
		{4 * wheelStepInterval, tea.MouseButtonWheelUp, 1},
		{5 * wheelStepInterval, tea.MouseButtonWheelUp, 0},
		{6 * wheelStepInterval, tea.MouseButtonWheelUp, 0},
	}
	for _, step := range steps {
		m.handleMouse(tea.MouseMsg{Button: step.button}, now.Add(step.after))
		if m.selected != step.want {
			t.Fatalf("at %s: got %d, want %d", step.after, m.selected, step.want)
		}
	}
	m.selected = len(m.order) - 1
	m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown}, now.Add(time.Second))
	if m.selected != len(m.order)-1 {
		t.Fatal("wheel moved beyond final service")
	}
}

func TestMouseDisabledDuringShutdownAndWithoutServices(t *testing.T) {
	for _, empty := range []bool{false, true} {
		m := mouseTestModel(t, 140, 40)
		x, y, _ := visibleServiceCell(t, m, "frontend")
		if empty {
			m.order = nil
		} else {
			m.shuttingDown = true
		}
		for _, button := range []tea.MouseButton{tea.MouseButtonLeft, tea.MouseButtonWheelDown} {
			updated, _ := m.Update(tea.MouseMsg{X: x, Y: y, Button: button})
			if updated.(model).selected != 0 {
				t.Fatal("mouse changed selection")
			}
		}
	}
}
