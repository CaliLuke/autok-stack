package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// Capture the actual opener arguments without launching a browser in tests.
func fakeBrowser(t *testing.T, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	capture := filepath.Join(dir, "opened")
	t.Setenv("STACK_TEST_OPENED", capture)
	for _, name := range []string{"open", "xdg-open"} {
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > \"$STACK_TEST_OPENED\"\nexit %d\n", exitCode)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return capture
}

func visibleURLCell(t *testing.T, m model, target string) (int, int) {
	t.Helper()
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	if len(lines) > m.height {
		lines = lines[len(lines)-m.height:]
	}
	for y, line := range lines {
		if pos := strings.Index(line, target); pos >= 0 {
			return ansi.StringWidth(line[:pos]), y
		}
	}
	t.Fatalf("URL %q not visible in:\n%s", target, m.View())
	return 0, 0
}

func TestClickOpensExactVisibleLogURL(t *testing.T) {
	for _, size := range [][2]int{{140, 40}, {100, 28}, {70, 20}, {60, 12}, {50, 10}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			capture := fakeBrowser(t, 0)
			m := mouseTestModel(t, size[0], size[1])
			m.selectedState().lastLogTail = "first http://localhost:3000/\n界 ➜ \x1b[32mhttps://localhost:4000/a_(b)?x=1&y=2\x1b[0m"
			target := "https://localhost:4000/a_(b)?x=1&y=2"
			x, y := visibleURLCell(t, m, target)
			updated, cmd := m.Update(tea.MouseMsg{X: x + 8, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			if cmd == nil {
				t.Fatal("click did not open URL")
			}
			updated, _ = updated.Update(cmd())
			if updated.(model).selected != m.selected || updated.(model).linkError != "" {
				t.Fatal("link click changed service or failed")
			}
			got, err := os.ReadFile(capture)
			if err != nil || string(got) != target+"\n" {
				t.Fatalf("opened %q, err %v; want %q", got, err, target)
			}
		})
	}
}

func TestOpenShortcutUsesFirstVisibleLink(t *testing.T) {
	capture := fakeBrowser(t, 0)
	m := mouseTestModel(t, 100, 28)
	m.selectedState().lastLogTail = "http://hidden.example\n" + strings.Repeat("later output\n", 30) + "ready (http://0.0.0.0:8000/docs).\nhttps://second.example"
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if cmd == nil {
		t.Fatal("shortcut did not open visible URL")
	}
	cmd()
	got, err := os.ReadFile(capture)
	if err != nil || string(got) != "http://localhost:8000/docs\n" {
		t.Fatalf("opened %q, err %v", got, err)
	}
}

func TestLinkOpeningIgnoresTruncationAndInactiveModes(t *testing.T) {
	for _, mode := range []string{"truncated", "selection", "info", "shutdown", "release", "motion", "right", "outside"} {
		t.Run(mode, func(t *testing.T) {
			m := mouseTestModel(t, 100, 28)
			m.selectedState().lastLogTail = "http://localhost:3000/"
			x, y := visibleURLCell(t, m, "http://localhost:3000/")
			click := tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
			switch mode {
			case "truncated":
				m.selectedState().lastLogTail += strings.Repeat("path", 100)
			case "selection":
				m.selectingText = true
			case "info":
				m.showingInfo = true
			case "shutdown":
				m.shuttingDown = true
			case "release":
				click.Action = tea.MouseActionRelease
			case "motion":
				click.Action = tea.MouseActionMotion
			case "right":
				click.Button = tea.MouseButtonRight
			case "outside":
				click.X--
			}
			if _, cmd := m.Update(click); cmd != nil {
				t.Fatal("unexpected link command")
			}
			if mode == "truncated" || mode == "selection" || mode == "info" || mode == "shutdown" {
				if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}}); cmd != nil {
					t.Fatal("unexpected shortcut command")
				}
			}
		})
	}
}

func TestBrowserFailureIsVisibleWithoutChangingServiceHealth(t *testing.T) {
	fakeBrowser(t, 1)
	m := mouseTestModel(t, 100, 28)
	m.selectedState().lastLogTail = "http://localhost:3000/"
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if cmd == nil {
		t.Fatal("missing open command")
	}
	updated, _ = updated.Update(cmd())
	m = updated.(model)
	if !strings.Contains(m.View(), "Could not open link") || m.anyExited || m.selectedState().exitErr != nil {
		t.Fatal("browser error missing or changed service health")
	}
}

func TestBrowserURLPreservesPathsAndNormalizesWildcardHosts(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"http://0.0.0.0:8000/docs", "http://localhost:8000/docs"},
		{"http://[::]:3000/?x=1#anchor", "http://localhost:3000/?x=1#anchor"},
		{"https://example.com/a_(b)?x=1&y=2", "https://example.com/a_(b)?x=1&y=2"},
		{"file:///tmp/file", ""},
		{"https://", ""},
	} {
		if got := browserURL(tc.raw); got != tc.want {
			t.Errorf("browserURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
