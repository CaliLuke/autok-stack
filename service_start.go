package main

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// startupServices includes the transitive dependencies of every automatic root.
func startupServices(configs []serviceConfig) map[string]bool {
	byKey := make(map[string]serviceConfig, len(configs))
	for _, cfg := range configs {
		byKey[cfg.Key] = cfg
	}
	wanted := make(map[string]bool, len(configs))
	var include func(string)
	include = func(key string) {
		if wanted[key] {
			return
		}
		wanted[key] = true
		for _, dependency := range byKey[key].DependsOn {
			include(dependency)
		}
	}
	for _, cfg := range configs {
		if !cfg.ManualStart {
			include(cfg.Key)
		}
	}
	return wanted
}

// Requests and dependency decisions stay on the model goroutine. Workers only
// receive config/process snapshots, and shared dependencies are queued once.
func (m *model) requestStart(key string) {
	state := m.services[key]
	if state == nil || state.startPending || state.starting || state.restarting || state.running {
		return
	}
	state.inactive = false
	state.startPending = true
	state.exitErr = nil
	state.nextRestartAt = time.Time{}
	for _, dependency := range state.config.DependsOn {
		m.requestStart(dependency)
	}
}

func (m model) advanceStarts(command tea.Cmd) (tea.Model, tea.Cmd) {
	if m.shuttingDown {
		return m, command
	}
	commands := []tea.Cmd{command}
	changed := false
	// Production order is topological, so a failure propagates through all
	// queued dependents in this pass, before any dependent process can launch.
	for _, key := range m.order {
		state := m.services[key]
		if state == nil || !state.startPending {
			continue
		}
		waiting := false
		var dependencyErr error
		for _, dependency := range state.config.DependsOn {
			dep := m.services[dependency]
			if dep != nil && (dep.startPending || dep.starting || dep.restarting) {
				waiting = true
				continue
			}
			if dep == nil || !dep.running || dep.exitErr != nil {
				dependencyErr = fmt.Errorf("dependency %s is not ready", dependency)
				if dep != nil && dep.exitErr != nil {
					dependencyErr = fmt.Errorf("dependency %s failed: %w", dependency, dep.exitErr)
				}
				break
			}
		}
		if dependencyErr != nil {
			state.startPending = false
			state.exitErr = dependencyErr
			state.lastLogTail = dependencyErr.Error()
			state.stoppedAt = time.Now()
			changed = true
			continue
		}
		if waiting {
			continue
		}
		state.startPending = false
		state.starting = true
		changed = true
		commands = append(commands, m.startServiceCmd(state))
	}
	if changed {
		m.anyExited = m.currentlyDegraded()
	}
	return m, tea.Batch(commands...)
}

// Use startup's compose-up and readiness behavior, with the same generation
// checks and operation tracking as manual restart and crash recovery.
func (m model) startServiceCmd(state *serviceState) tea.Cmd {
	cfg, previous, generation := state.config, state.process, state.generation+1
	return m.operations.command(func() tea.Msg {
		result := restartResultMsg{key: cfg.Key, generation: generation, oldStopped: previous == nil}
		if err := previous.stop(m.runtimeCtx); err != nil {
			result.err = err
			return result
		}
		result.oldStopped = true
		next, err := startServiceContext(m.runtimeCtx, cfg, m.exitCh, generation, m.maxLogBytes)
		if err != nil {
			result.err = fmt.Errorf("start: %w", err)
			return result
		}
		m.operations.track(next)
		process := next.process
		next, err = finishBootService(m.runtimeCtx, next.config, next)
		if next.process != process {
			m.operations.release(process)
		}
		next.generation = generation
		result.next, result.err = next, err
		return result
	})
}
