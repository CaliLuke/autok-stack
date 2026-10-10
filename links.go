package main

import (
	"context"
	"net"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const linkHint = "click URL · o first link · m select text"
const textSelectionHint = "select text / open links · m resume · q quit"

var logURLPattern = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `\x00-\x1f│]+`)

type visibleLink struct {
	url        string
	x, endX, y int
}

type linkOpenedMsg struct{ err error }

// Match complete URLs from the log against the actual terminal cells. Using
// the final render accounts for borders, Unicode, and bottom-clipped views.
// A URL truncated by the log panel must never open a different, partial URL.
func (m model) visibleLinks() []visibleLink {
	state := m.selectedState()
	if state == nil || state.inactive || m.width <= 0 || m.height <= 0 {
		return nil
	}
	targets := make(map[string]string)
	for _, raw := range logURLPattern.FindAllString(ansi.Strip(state.lastLogTail), -1) {
		raw = trimURLPunctuation(raw)
		if target := browserURL(raw); target != "" {
			targets[raw] = target
		}
	}
	if len(targets) == 0 {
		return nil
	}
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	if len(lines) > m.height {
		lines = lines[len(lines)-m.height:]
	}
	var links []visibleLink
	for y, line := range lines {
		for _, span := range logURLPattern.FindAllStringIndex(line, -1) {
			raw := trimURLPunctuation(line[span[0]:span[1]])
			target, ok := targets[raw]
			if !ok {
				continue
			}
			x := ansi.StringWidth(line[:span[0]])
			endX := x + ansi.StringWidth(raw)
			if endX <= m.width {
				links = append(links, visibleLink{url: target, x: x, endX: endX, y: y})
			}
		}
	}
	return links
}

func trimURLPunctuation(raw string) string {
	raw = strings.TrimRight(raw, ".,;!?")
	for _, pair := range [][2]string{{"(", ")"}, {"[", "]"}, {"{", "}"}} {
		for strings.HasSuffix(raw, pair[1]) && strings.Count(raw, pair[1]) > strings.Count(raw, pair[0]) {
			raw = strings.TrimSuffix(raw, pair[1])
		}
	}
	return raw
}

func browserURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return ""
	}
	// Servers advertise wildcard bind addresses, which are not browser targets.
	if host := parsed.Hostname(); host == "0.0.0.0" || host == "::" {
		port := parsed.Port()
		parsed.Host = "localhost"
		if port != "" {
			parsed.Host = net.JoinHostPort("localhost", port)
		}
	}
	return parsed.String()
}

func openLinkCmd(target string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		name := "xdg-open"
		if runtime.GOOS == "darwin" {
			name = "open"
		}
		// Pass the URL as one argument, never through a shell. Keep opener output
		// away from the alternate screen and report failures through Update.
		return linkOpenedMsg{err: exec.CommandContext(ctx, name, target).Run()}
	}
}
