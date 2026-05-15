---
name: tui-snapshot-iteration
description: Iterate on the visual polish of a Bubble Tea (or any lipgloss-based) TUI from inside a non-interactive agent shell where the running terminal UI can't actually be seen. Use when the user asks to "polish the TUI", "improve the UI of my Bubble Tea app", "iterate on the dashboard look", "fix alignment / spacing / colors in the TUI", or when you realize you've been asked to do visual work on a TUI without a way to view it. Builds a snapshot-test harness that renders the model at chosen widths/heights with the lipgloss color profile forced to ASCII, writes the rendered text to disk, and lets the agent read it back to spot structural issues, edit, and re-snapshot — converging without depending on a human's eyes for every round.
---

# Iterating on a Bubble Tea TUI without seeing it

## When to use this

The user wants visual polish on a TUI (alignment, spacing, hierarchy, badge layouts, hero bars, log panes, list selection markers). You're in a Claude Code / agent shell. Spawning the TUI yourself doesn't help — Bubble Tea takes over the terminal with alt-screen and raw mode, and your Bash tool's output is a stream of escape codes you can't visually evaluate. Asking the user to describe every iteration is slow. Live-reload tools like `air` only pay off if a human is staring at the terminal.

This skill replaces the human-in-the-loop with a snapshot harness: render `model.View()` directly at chosen sizes, strip ANSI, write plain-text scenes to disk, read them back, identify what's wrong, edit, re-snapshot.

If the user is sitting at a terminal and _wants_ to drive iteration interactively, prefer setting up `air` for them. This skill is for the autonomous case.

## The constraint, named explicitly

Be upfront with the user: **a TUI cannot be visually evaluated from a non-interactive shell.** Snapshots reveal:

- ✅ Layout — column alignment, panel sizes, gutter widths, wrapping, truncation
- ✅ Text — labels, separators, what gets shown when, ordering
- ✅ Structural state changes — what a "selected row" looks like compared to non-selected, what changes when something is in `restart` vs `exit` vs `live`
- ✅ Edge sizes — what the layout does at 50×10, 70×20, 140×40

They do **not** reveal:

- ❌ Colors, contrast, theme cohesion
- ❌ Anything that depends on terminal capabilities (truecolor vs 256-color degradation)
- ❌ Cursor flicker, redraw artifacts, real-time behavior

So: get the structural polish dialled in with snapshots, then ask the user to install the binary and eyeball it once in a real terminal to validate colors.

## The recipe

### 1. Write a snapshot test

In the same package as the `model` (you need access to unexported types), create `snapshot_test.go`. The template at the end of this skill is the version that was used to polish `autok-stack`'s dashboard.

The test should:

1. **Force lipgloss to ASCII** with `lipgloss.SetDefaultRenderer(lipgloss.NewRenderer(os.Stderr, termenv.WithProfile(termenv.Ascii)))`. This strips color escape sequences out of the rendered string. (lipgloss still emits cursor-positioning / dimension escapes — strip those with a regex; see template.)
2. **Build a representative model** by hand, with a small set of services / items / state in mutually distinct conditions. Don't bootstrap real subprocesses; just fill the struct fields.
3. **Define scenes** as `(name, width, height, mutate func)` rows. Cover at minimum: a healthy wide view, a mid-width view, a narrow view, the smallest size that triggers any compact-fallback path, and one "everything is on fire" scene with mixed states.
4. **Render and write**: for each scene, set `m.width` / `m.height`, optionally `mutate(&m)`, call `m.View()`, strip ANSI, write to `.tmp/snapshots/<scene>.txt`.
5. **Gitignore** `.tmp/`.

Run with:

```bash
go test -run TestUISnapshot -v
```

### 2. Read the snapshots

Open each one. Look for:

- **Alignment drift** — does the header column position match the row column position? Does the selected row visibly shift the layout?
- **Selected-row visual delta** — if your selected row uses a border or extra padding, it changes height and breaks vertical alignment of the rest of the list. Almost always wrong; prefer a 1-line indicator (accent bar, bg highlight, bold name) over a multi-line box.
- **Hero / footer bars** — are they actually styled, or is the style struct declared but never applied? It's easy for a `m.styles.hero` to be defined and then `renderHero` calls `lipgloss.NewStyle().Width(width)` instead of `m.styles.hero.Width(width)`, so the styling silently disappears.
- **Phantom whitespace** — `MarginTop(1)` / `MarginBottom(1)` on a selected style adds blank lines that move other content. The snapshot will show this as a stray gap.
- **Verbose strings in tight columns** — `1h0m0s` in a uptime column where `1h` is fine; full compose commands when the service list is the meaningful info.
- **Duplicate semantics** — if `exit`, `down`, and `restart` all use the same `warn` color, three states are visually indistinguishable. Snapshots can't show the color, but you can see that all three would resolve to the same `style.Render(" label ")` chip in code.
- **Empty / wasted regions** — log pane filling 24 rows with 2 rows of content. Sometimes acceptable (the pane _is_ the viewport), sometimes worth tightening.

### 3. Edit, re-snapshot, diff

Make targeted changes (one issue at a time when uncertain; a small batch when confident). Re-run the test. Open the snapshot. Did the fix land? Did anything else move that shouldn't have?

Don't try to fix every issue in one pass. The snapshot is cheap; iteration is the leverage.

### 4. Hand off for color review

When the structure is right, install the binary, ask the user to launch it once in a real terminal, and ask specifically: _"are the chip colors distinct enough?", "does the selected-row tint stand out?", "is the title-bar accent OK against your terminal background?"_ — color-specific questions, not vague "does it look good?".

## What good "scenes" look like

For a service-dashboard style TUI, the scenes used on `autok-stack` were:

| Scene                  | Size   | What it exposed                                                                                               |
| ---------------------- | ------ | ------------------------------------------------------------------------------------------------------------- |
| `wide-healthy`         | 140×40 | The happy path layout                                                                                         |
| `medium-healthy`       | 100×28 | Whether the layout still breathes at a common terminal width                                                  |
| `narrow-healthy`       | 70×20  | When does truncation start; does the focus line wrap                                                          |
| `compact-fallback`     | 50×10  | The "too small, render the alternate view" branch                                                             |
| `wide-mixed-states`    | 140×40 | All status chips visible at once: live, restart, exit, down                                                   |
| `wide-selected-server` | 140×40 | Mid-list selection (catches "phantom gap" between header and row 0 that only surfaces when row 0 is selected) |

Pick scenes that hit _boundary conditions_, not just variations of "healthy".

## Pitfalls

- **Don't strip _all_ escape codes blindly.** Some `\x1b[` sequences carry layout info (cursor moves). Strip only color/style sequences (`\x1b[[0-9;]*[A-Za-z]` is the catch-all but be aware it can be over-aggressive — verify the snapshot still has visible box-drawing characters).
- **Trailing whitespace** on each line makes diffs noisy. Trim it after stripping ANSI.
- **lipgloss renderer is global state.** Setting `SetDefaultRenderer` to ASCII inside one test affects the whole process. Fine in isolation; surprising if you ever mix snapshot tests with tests that care about real ANSI.
- **The harness only sees `View()`.** It can't tell you whether `Update()` does the right thing on keypress. If you want to validate keypress-driven state changes, drive the model with `tea.KeyMsg{}` values manually before calling `View()`, or step up to `github.com/charmbracelet/x/exp/teatest`.
- **Don't refactor the production model just to make it testable.** The snapshot test can construct the model directly from internal fields; it doesn't need a public factory.

## Template

```go
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

// TestUISnapshot renders the TUI at chosen sizes/states with color forced to
// ASCII and writes the result to .tmp/snapshots/<scene>.txt so the rendered UI
// can be inspected without a real terminal.
//
// Run with: go test -run TestUISnapshot -v
func TestUISnapshot(t *testing.T) {
 lipgloss.SetDefaultRenderer(
  lipgloss.NewRenderer(os.Stderr, termenv.WithProfile(termenv.Ascii)),
 )

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
   // flip some items into degraded states so every chip variant renders
  }},
  {name: "wide-mid-selection", width: 140, height: 40, mutate: func(m *model) {
   m.selected = len(m.order) / 2
  }},
 }

 outDir := ".tmp/snapshots"
 if err := os.MkdirAll(outDir, 0o755); err != nil {
  t.Fatal(err)
 }

 for _, tc := range cases {
  m := newFakeModel() // hand-build with representative data
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
 // Hand-build a model with representative items in distinct states.
 // Fill startedAt with a real time so uptime renders.
 now := time.Now()
 _ = now
 return model{ /* ... fill fields directly ... */ }
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string {
 clean := ansiRE.ReplaceAllString(s, "")
 lines := strings.Split(clean, "\n")
 for i, l := range lines {
  lines[i] = strings.TrimRight(l, " ")
 }
 return strings.Join(lines, "\n")
}
```

The working implementation against `autok-stack`'s actual model lives at `snapshot_test.go` in this repo; use it as a reference, not boilerplate to copy unchanged.

## Workflow summary

1. State the constraint to the user. Offer the snapshot approach as the autonomous path.
2. Write `snapshot_test.go` with hand-built model + ASCII renderer + ANSI strip + scenes covering boundary conditions.
3. Run, read snapshots, identify structural issues.
4. Edit in small focused passes; re-snapshot; verify the fix landed and nothing else moved.
5. Hand off to the user for color review in a real terminal — ask specific color/contrast questions.
6. Leave the harness committed and `.tmp/` gitignored so future iterations are one `go test` away.
