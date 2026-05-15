package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type serviceConfig struct {
	Key             string
	Name            string
	Ports           []string
	WorkDir         string
	LogFile         string
	Command         []string
	ComposeFile     string
	ComposeServices []string
	// AutoRestart enables exponential-backoff restart of crashed processes.
	AutoRestart bool
	// ReadinessTimeout bounds how long startup waits for the service to bind its first port.
	ReadinessTimeout time.Duration
}

type serviceState struct {
	config         serviceConfig
	cmd            *exec.Cmd
	pid            int
	running        bool
	restarting     bool
	ignoredExits   map[int]struct{}
	exitErr        error
	startedAt      time.Time
	stoppedAt      time.Time
	lastLogTail    string
	restartAttempt int
	nextRestartAt  time.Time
	composeDown    bool
}

type processExitMsg struct {
	key string
	pid int
	err error
}

type composeServiceStatus struct {
	Service  string `json:"Service"`
	Name     string `json:"Name"`
	State    string `json:"State"`
	Status   string `json:"Status"`
	Health   string `json:"Health"`
	ExitCode int    `json:"ExitCode"`
}

type tickMsg time.Time
type spinnerTickMsg time.Time

// spinnerFrames cycles through 10 dots-pattern braille glyphs.
var spinnerFrames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

func spinnerGlyph(frame int) string {
	if frame < 0 {
		frame = -frame
	}
	return string(spinnerFrames[frame%len(spinnerFrames)])
}

type palette struct {
	bg       lipgloss.Color
	panelAlt lipgloss.Color
	border   lipgloss.Color
	muted    lipgloss.Color
	text     lipgloss.Color
	accent   lipgloss.Color
	success  lipgloss.Color
	warn     lipgloss.Color
	danger   lipgloss.Color
}

type styles struct {
	app            lipgloss.Style
	heroHealthy    lipgloss.Style
	heroDegraded   lipgloss.Style
	heroTitle      lipgloss.Style
	listHeader     lipgloss.Style
	listSelected   lipgloss.Style
	selectedAccent lipgloss.Style
	kpiValue       lipgloss.Style
	muted          lipgloss.Style
	// Row chips: fg-colored glyph + label, no background pill.
	chipUp     lipgloss.Style
	chipWarn   lipgloss.Style
	chipDown   lipgloss.Style
	chipDanger lipgloss.Style
	// Hero badge: solid bg pill; flashes between danger/warn when degraded.
	badgeHealthy lipgloss.Style
	badgeWarn    lipgloss.Style
	badgeDanger  lipgloss.Style
	// Used for focus-line error banner and other inline-warning surfaces.
	statusDanger lipgloss.Style
	logBox       lipgloss.Style
}

type model struct {
	rootDir      string
	logDir       string
	title        string
	maxLogBytes  int64
	services     map[string]*serviceState
	order        []string
	exitCh       chan processExitMsg
	shuttingDown bool
	anyExited    bool
	selected     int
	width        int
	height       int
	styles       styles
	spinnerFrame int
}

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to determine cwd: %v\n", err)
		os.Exit(1)
	}

	cfg, err := loadConfig(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	maxLogBytes, err := parseMaxLogBytes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid MAX_LOG_BYTES: %v\n", err)
		os.Exit(1)
	}

	for _, tool := range cfg.requiredTools {
		if _, err := exec.LookPath(tool); err != nil {
			fmt.Fprintf(os.Stderr, "%s is required but was not found in PATH\n", tool)
			os.Exit(1)
		}
	}

	if err := os.MkdirAll(cfg.logDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create log dir: %v\n", err)
		os.Exit(1)
	}

	releaseLock, err := acquireSingletonLock(cfg.rootDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer releaseLock()

	cleanupStaleHelpers(cfg.cleanupPatterns)

	if err := truncateLogs(cfg.services); err != nil {
		fmt.Fprintf(os.Stderr, "failed to prepare logs: %v\n", err)
		os.Exit(1)
	}

	exitCh := make(chan processExitMsg, len(cfg.services)*2)
	services := make(map[string]*serviceState, len(cfg.services))
	order := make([]string, 0, len(cfg.services))

	for _, sc := range cfg.services {
		state, err := startService(sc, exitCh)
		if err != nil {
			_ = shutdownServices(services, order)
			fmt.Fprintf(os.Stderr, "failed to start %s: %v\n", sc.Name, err)
			os.Exit(1)
		}
		services[sc.Key] = state
		order = append(order, sc.Key)
		if err := capLogFile(sc.LogFile, maxLogBytes); err != nil {
			_ = shutdownServices(services, order)
			fmt.Fprintf(os.Stderr, "failed to trim log %s: %v\n", sc.LogFile, err)
			os.Exit(1)
		}
		if sc.isComposeService() {
			if err := refreshComposeService(state); err != nil {
				_ = shutdownServices(services, order)
				fmt.Fprintf(os.Stderr, "failed to refresh %s: %v\n", sc.Name, err)
				os.Exit(1)
			}
		} else {
			state.lastLogTail = readLogTail(sc.LogFile, 18)
			if err := waitForReadiness(sc); err != nil {
				// Readiness failure is informational — the service may still come up.
				// Surface it in the log tail so the dashboard reflects reality.
				state.lastLogTail = strings.TrimSpace(state.lastLogTail+"\n[stack] readiness: "+err.Error()) + "\n"
			}
		}
	}

	m := model{
		rootDir:     cfg.rootDir,
		logDir:      cfg.logDir,
		title:       cfg.title,
		maxLogBytes: maxLogBytes,
		services:    services,
		order:       order,
		exitCh:      exitCh,
		styles:      newStyles(),
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		_ = shutdownServices(services, order)
		releaseLock()
		os.Exit(130)
	}()

	program := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := program.Run()
	shutdownErr := shutdownServices(services, order)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stack terminal failed: %v\n", err)
		os.Exit(1)
	}
	if shutdownErr != nil {
		fmt.Fprintf(os.Stderr, "failed to stop services cleanly: %v\n", shutdownErr)
		os.Exit(1)
	}

	if fm, ok := finalModel.(model); ok && fm.anyExited {
		os.Exit(1)
	}
}

// acquireSingletonLock ensures only one stack instance runs against this repo.
// Self-heals stale locks (dead PID, recycled PID, missing file). When the recorded
// PID is a live `stack`, prompts the user to take over: replying "y" SIGTERMs the
// existing instance and waits for it to clean up; anything else aborts.
func acquireSingletonLock(rootDir string) (func(), error) {
	lockDir := filepath.Join(rootDir, ".tmp", "dev-stack")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(lockDir, "stack.pid")

	if data, err := os.ReadFile(lockPath); err == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil && pid > 0 && pid != os.Getpid() {
			if isLiveStackProcess(pid) {
				if !promptTakeover(pid) {
					return nil, fmt.Errorf("another stack is already running (pid %d)", pid)
				}
				if err := terminateAndWait(pid, 10*time.Second); err != nil {
					return nil, fmt.Errorf("failed to stop existing stack (pid %d): %w", pid, err)
				}
			}
		}
	}

	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(lockPath) }, nil
}

// promptTakeover asks the user whether to kill the existing stack. Defaults to no.
func promptTakeover(pid int) bool {
	fmt.Fprintf(os.Stderr, "Another stack is already running (pid %d). Kill it and take over? [y/N] ", pid)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// terminateAndWait SIGTERMs pid and polls until it exits or timeout, then SIGKILLs.
func terminateAndWait(pid int, timeout time.Duration) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	// Brief wait so the OS reaps the process group before we re-bind ports.
	time.Sleep(500 * time.Millisecond)
	return nil
}

// isLiveStackProcess reports whether pid is alive AND is a `stack` binary.
// Falls back to "alive" if we can't read the process command (defensive, since
// a false positive only delays a manual override; a false negative would let
// two stacks fight over services).
func isLiveStackProcess(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return true
	}
	return strings.HasSuffix(strings.TrimSpace(string(out)), "stack")
}

func parseMaxLogBytes() (int64, error) {
	raw := os.Getenv("MAX_LOG_BYTES")
	if raw == "" {
		return 1048576, nil
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, fmt.Errorf("must be greater than 0")
	}
	return value, nil
}

func truncateLogs(configs []serviceConfig) error {
	for _, cfg := range configs {
		if err := os.WriteFile(cfg.LogFile, nil, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// cleanupStaleHelpers best-effort kills helper scripts left over from a previous run.
// Failures here are non-fatal — `ensurePortsAvailable` runs per-service and will free
// any port we still need. Aborting startup over a `pkill` quirk is worse than the
// stale process surviving.
func cleanupStaleHelpers(patterns []string) {
	for _, pattern := range patterns {
		_ = exec.Command("pkill", "-f", pattern).Run()
	}
}

func startService(cfg serviceConfig, exitCh chan<- processExitMsg) (*serviceState, error) {
	if cfg.isComposeService() {
		return startComposeService(cfg)
	}

	if err := ensurePortsAvailable(cfg.Ports); err != nil {
		return nil, err
	}

	logFile, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = cfg.WorkDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}

	state := &serviceState{
		config:       cfg,
		cmd:          cmd,
		pid:          cmd.Process.Pid,
		running:      true,
		startedAt:    time.Now(),
		ignoredExits: make(map[int]struct{}),
	}

	go func(key string, child *exec.Cmd, file *os.File) {
		err := child.Wait()
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		exitCh <- processExitMsg{key: key, pid: child.Process.Pid, err: err}
	}(cfg.Key, cmd, logFile)

	return state, nil
}

func startComposeService(cfg serviceConfig) (*serviceState, error) {
	if err := ensureComposeServicesUp(cfg); err != nil {
		return nil, err
	}

	state := &serviceState{
		config:    cfg,
		running:   true,
		startedAt: time.Now(),
	}
	if err := refreshComposeService(state); err != nil {
		return nil, err
	}
	return state, nil
}

// shutdownServices stops every non-compose service the model is tracking.
// Order is intentionally the reverse of `order` so consumers stop before producers
// (e.g. frontend before frontend-log). Compose services are intentionally left
// running — quitting the dashboard should not tear down Postgres/TypeDB.
func shutdownServices(services map[string]*serviceState, order []string) error {
	var errs []error
	for i := len(order) - 1; i >= 0; i-- {
		state, ok := services[order[i]]
		if !ok || state == nil || state.config.isComposeService() || state.cmd == nil || state.cmd.Process == nil {
			continue
		}
		if err := stopProcessGroup(state.cmd.Process.Pid); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", state.config.Name, err))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

func stopProcessGroup(pid int) error {
	if pid <= 0 {
		return nil
	}

	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(-pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// waitForReadiness blocks until the service's first declared port accepts a TCP
// connection, or the configured timeout elapses. Services with no ports return
// immediately. This replaces the previous fixed 1-second inter-start sleep.
func waitForReadiness(cfg serviceConfig) error {
	if len(cfg.Ports) == 0 {
		return nil
	}
	timeout := cfg.ReadinessTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	port := cfg.Ports[0]
	addr := net.JoinHostPort("127.0.0.1", port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("port %s did not become ready within %s", port, timeout)
}

func ensurePortsAvailable(ports []string) error {
	if len(ports) == 0 {
		return nil
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		return fmt.Errorf("lsof is required to free service ports: %w", err)
	}

	pids := make(map[int]struct{})
	for _, port := range ports {
		pidOutput, err := exec.Command("lsof", "-tiTCP:"+port, "-sTCP:LISTEN").Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				continue
			}
			return fmt.Errorf("inspect port %s: %w", port, err)
		}

		for _, line := range strings.Split(strings.TrimSpace(string(pidOutput)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil || pid <= 0 {
				continue
			}
			pids[pid] = struct{}{}
		}
	}

	for pid := range pids {
		if err := stopPID(pid); err != nil {
			return fmt.Errorf("stop pid %d: %w", pid, err)
		}
	}

	return nil
}

func stopPID(pid int) error {
	if pid <= 0 {
		return nil
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (cfg serviceConfig) isComposeService() bool {
	return len(cfg.ComposeServices) > 0
}

func (cfg serviceConfig) composeBaseArgs() []string {
	return []string{"compose", "-f", filepath.Base(cfg.ComposeFile)}
}

func (cfg serviceConfig) commandText() string {
	if cfg.isComposeService() {
		return "compose: " + strings.Join(cfg.ComposeServices, " ")
	}
	return strings.Join(cfg.Command, " ")
}

func ensureComposeServicesUp(cfg serviceConfig) error {
	statuses, err := readComposeStatuses(cfg)
	if err != nil {
		return err
	}
	if composeServicesRunning(cfg, statuses) {
		return nil
	}

	args := append(cfg.composeBaseArgs(), append([]string{"up", "-d"}, cfg.ComposeServices...)...)
	cmd := exec.Command("podman", args...)
	cmd.Dir = cfg.WorkDir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start compose services: %w%s", err, formatCommandOutput(output))
	}

	statuses, err = readComposeStatuses(cfg)
	if err != nil {
		return err
	}
	if !composeServicesRunning(cfg, statuses) {
		return fmt.Errorf("compose services did not reach running state")
	}
	return nil
}

func restartComposeServices(cfg serviceConfig) error {
	args := append(cfg.composeBaseArgs(), append([]string{"restart"}, cfg.ComposeServices...)...)
	cmd := exec.Command("podman", args...)
	cmd.Dir = cfg.WorkDir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restart compose services: %w%s", err, formatCommandOutput(output))
	}
	return ensureComposeServicesUp(cfg)
}

func readComposeStatuses(cfg serviceConfig) ([]composeServiceStatus, error) {
	args := append(cfg.composeBaseArgs(), append([]string{"ps", "--format", "json"}, cfg.ComposeServices...)...)
	cmd := exec.Command("podman", args...)
	cmd.Dir = cfg.WorkDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read compose status: %w%s", err, formatCommandOutput(stderr.Bytes()))
	}

	// Newer podman/docker compose emits a JSON array; older versions emit one
	// JSON object per line (NDJSON). Detect by the first non-whitespace byte.
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return nil, nil
	}

	var statuses []composeServiceStatus
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &statuses); err != nil {
			return nil, fmt.Errorf("parse compose status (array): %w", err)
		}
		return statuses, nil
	}

	for _, line := range strings.Split(string(trimmed), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var status composeServiceStatus
		if err := json.Unmarshal([]byte(line), &status); err != nil {
			return nil, fmt.Errorf("parse compose status: %w", err)
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func composeServicesRunning(cfg serviceConfig, statuses []composeServiceStatus) bool {
	if len(statuses) < len(cfg.ComposeServices) {
		return false
	}

	byService := make(map[string]composeServiceStatus, len(statuses))
	for _, status := range statuses {
		byService[status.Service] = status
	}

	for _, service := range cfg.ComposeServices {
		status, ok := byService[service]
		if !ok {
			return false
		}
		if !strings.EqualFold(status.State, "running") {
			return false
		}
	}

	return true
}

func refreshComposeService(state *serviceState) error {
	statuses, err := readComposeStatuses(state.config)
	if err != nil {
		state.running = false
		state.exitErr = err
		state.lastLogTail = err.Error()
		state.stoppedAt = time.Now()
		return err
	}

	state.running = composeServicesRunning(state.config, statuses)
	if state.running {
		state.exitErr = nil
		state.stoppedAt = time.Time{}
	} else {
		state.exitErr = fmt.Errorf("one or more compose services are not running")
		state.stoppedAt = time.Now()
	}
	state.lastLogTail = composeStatusSummary(state.config, statuses)
	return nil
}

func composeStatusSummary(cfg serviceConfig, statuses []composeServiceStatus) string {
	if len(statuses) == 0 {
		return "No compose containers found for this service."
	}

	byService := make(map[string]composeServiceStatus, len(statuses))
	for _, status := range statuses {
		byService[status.Service] = status
	}

	lines := make([]string, 0, len(cfg.ComposeServices))
	for _, service := range cfg.ComposeServices {
		status, ok := byService[service]
		if !ok {
			lines = append(lines, fmt.Sprintf("%-10s missing", service))
			continue
		}
		summary := fmt.Sprintf("%-10s %-7s %s", service, status.State, status.Status)
		if status.Health != "" {
			summary += " health=" + status.Health
		}
		lines = append(lines, summary)
	}

	return strings.Join(lines, "\n")
}

func formatCommandOutput(output []byte) string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return ""
	}
	return ": " + text
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func spinnerTickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerTickMsg(t)
	})
}

func waitForExitCmd(ch <-chan processExitMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), spinnerTickCmd(), waitForExitCmd(m.exitCh))
}

// anyAnimating reports whether something on screen wants a fast redraw —
// either a spinner is spinning (something is restarting) or the degraded
// badge is flashing (anyExited). Avoids burning a 100ms tick when the
// dashboard is fully healthy and static.
func (m model) anyAnimating() bool {
	if m.anyExited {
		return true
	}
	for _, s := range m.services {
		if s != nil && s.restarting {
			return true
		}
	}
	return false
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.shuttingDown = true
			return m, tea.Quit
		case "up", "k":
			m.selected = max(0, m.selected-1)
			m.refreshSelectedLog()
		case "down", "j":
			m.selected = min(len(m.order)-1, m.selected+1)
			m.refreshSelectedLog()
		case "g", "home":
			m.selected = 0
			m.refreshSelectedLog()
		case "G", "end":
			if len(m.order) > 0 {
				m.selected = len(m.order) - 1
			}
			m.refreshSelectedLog()
		case "r":
			if err := m.restartSelectedService(); err != nil {
				state := m.selectedState()
				state.exitErr = err
				state.running = false
				state.restarting = false
				if state.config.isComposeService() {
					state.lastLogTail = err.Error()
				} else {
					state.lastLogTail = readLogTail(state.config.LogFile, 18)
				}
				m.anyExited = true
			}
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case spinnerTickMsg:
		m.spinnerFrame++
		if m.anyAnimating() {
			return m, spinnerTickCmd()
		}
		// Schedule one more tick so we resume animation as soon as state changes.
		return m, tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
			return spinnerTickMsg(t)
		})
	case tickMsg:
		now := time.Now()
		for _, state := range m.services {
			if state.config.isComposeService() {
				if err := refreshComposeService(state); err != nil {
					// A real podman/CLI failure is exceptional.
					m.anyExited = true
				}
				// A compose service merely being "not running" right now
				// (e.g. mid-restart) is reported on the dashboard but does
				// not flip the global failure flag — that's reserved for
				// errors the user actually needs to investigate.
				continue
			}
			if err := capLogFile(state.config.LogFile, m.maxLogBytes); err != nil {
				state.exitErr = err
				state.running = false
				state.stoppedAt = now
				m.anyExited = true
			}
			state.lastLogTail = readLogTail(state.config.LogFile, 18)

			if state.config.AutoRestart && !state.running && !state.restarting && !m.shuttingDown {
				if !state.nextRestartAt.IsZero() && now.Before(state.nextRestartAt) {
					continue
				}
				if err := m.autoRestartService(state); err != nil {
					state.exitErr = err
					m.anyExited = true
				}
			}
		}
		return m, tickCmd()
	case processExitMsg:
		state, ok := m.services[msg.key]
		if ok {
			if _, ignored := state.ignoredExits[msg.pid]; ignored {
				delete(state.ignoredExits, msg.pid)
				return m, waitForExitCmd(m.exitCh)
			}
			if state.pid != msg.pid {
				return m, waitForExitCmd(m.exitCh)
			}
			state.running = false
			state.restarting = false
			state.exitErr = msg.err
			state.stoppedAt = time.Now()
			state.lastLogTail = readLogTail(state.config.LogFile, 18)
			if state.config.AutoRestart && !m.shuttingDown {
				state.restartAttempt++
				state.nextRestartAt = time.Now().Add(backoffDelay(state.restartAttempt))
				return m, waitForExitCmd(m.exitCh)
			}
		}
		if !m.shuttingDown {
			m.anyExited = true
		}
		return m, waitForExitCmd(m.exitCh)
	}

	return m, nil
}

func (m model) View() string {
	if m.width < 60 || m.height < 12 {
		return m.renderCompactFallback()
	}

	selected := m.selectedState()
	width := max(60, m.width-2)

	hero := m.renderHero(width)
	list := m.renderServiceList(width)
	focus := m.renderFocusLine(selected, width)
	footer := m.renderFooter(width)

	used := lipgloss.Height(hero) +
		lipgloss.Height(list) +
		lipgloss.Height(focus) +
		lipgloss.Height(footer) +
		2 // app vertical padding
	logHeight := max(4, m.height-used)

	logs := m.renderLogPanel(selected, width, logHeight)
	layout := lipgloss.JoinVertical(lipgloss.Left, hero, list, focus, logs, footer)
	return m.styles.app.Render(layout)
}

func (m model) renderCompactFallback() string {
	selected := m.selectedState()
	var b strings.Builder
	b.WriteString(m.title + "\n")
	b.WriteString("resize terminal for dashboard mode\n\n")
	for i, key := range m.order {
		state := m.services[key]
		cursor := " "
		if i == m.selected {
			cursor = "▌"
		}
		status := "◯ down"
		switch {
		case state.restarting:
			status = spinnerGlyph(m.spinnerFrame) + " restart"
		case state.running:
			status = "● live"
		case state.exitErr != nil:
			status = "✕ exit"
		}
		b.WriteString(fmt.Sprintf("%s %-10s %-10s pid=%d ports=%s\n", cursor, state.config.Name, status, state.pid, strings.Join(state.config.Ports, ",")))
	}
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("selected: %s\n", selected.config.LogFile))
	if selected.lastLogTail == "" {
		b.WriteString("no log output yet\n")
	} else {
		b.WriteString(selected.lastLogTail)
	}
	return b.String()
}

func (m model) renderHero(width int) string {
	upCount := 0
	for _, key := range m.order {
		if state := m.services[key]; state != nil && state.running {
			upCount++
		}
	}

	// Healthy: solid green pill. Degraded: alternate danger/warn each 500ms
	// (5 spinner frames) so the eye is drawn to the count without it being
	// a frenetic flash.
	badgeText := fmt.Sprintf(" %d/%d live ", upCount, len(m.order))
	var badge string
	if !m.anyExited {
		badge = m.styles.badgeHealthy.Render(badgeText)
	} else if (m.spinnerFrame/5)%2 == 0 {
		badge = m.styles.badgeDanger.Render(badgeText)
	} else {
		badge = m.styles.badgeWarn.Render(badgeText)
	}

	title := m.styles.heroTitle.Render(m.title)

	// Middle: show the currently selected service so the eye has a target.
	var middle string
	if sel := m.selectedState(); sel != nil && sel.config.Name != "" {
		middle = m.styles.muted.Render("focus ") + m.styles.kpiValue.Render(sel.config.Name)
	}

	meta := m.styles.muted.Render(fmt.Sprintf("log cap %s", humanBytes(m.maxLogBytes)))

	left := lipgloss.JoinHorizontal(lipgloss.Center, title, "  ", badge)
	leftW := lipgloss.Width(left)
	middleW := lipgloss.Width(middle)
	metaW := lipgloss.Width(meta)
	// 6 = 2 (border) + 4 (horizontal padding inside the border, 2 each side).
	// Distribute remaining space: half before middle, half after.
	remaining := width - leftW - middleW - metaW - 6
	if remaining < 2 {
		remaining = 2
	}
	leftGap := remaining / 2
	rightGap := remaining - leftGap
	if leftGap < 1 {
		leftGap = 1
	}
	if rightGap < 1 {
		rightGap = 1
	}
	content := left + strings.Repeat(" ", leftGap) + middle + strings.Repeat(" ", rightGap) + meta

	heroStyle := m.styles.heroHealthy
	if m.anyExited {
		heroStyle = m.styles.heroDegraded
	}
	return heroStyle.Width(width).Render(content)
}

// columnWidths derives shared column widths for the service list so the header
// and rows stay aligned regardless of name/port lengths in the running stack.
func (m model) columnWidths(width int) (nameW, portsW, pidW, ageW, statusW int) {
	nameW = 6
	portsW = 5
	for _, key := range m.order {
		state := m.services[key]
		if state == nil {
			continue
		}
		if n := len(state.config.Name); n > nameW {
			nameW = n
		}
		if p := len(strings.Join(state.config.Ports, ",")); p > portsW {
			portsW = p
		}
	}
	pidW = 7
	ageW = 8 // fits "1m 35s" / "2d 4h" with the new space separator
	statusW = 9 // fits the longest chip: "⠋ restart"
	// Guard against absurd port lists eating the entire row.
	maxPorts := max(8, width-nameW-pidW-ageW-statusW-12)
	if portsW > maxPorts {
		portsW = maxPorts
	}
	return
}

func (m model) renderServiceList(width int) string {
	nameW, portsW, pidW, ageW, statusW := m.columnWidths(width)

	headerFmt := fmt.Sprintf("  %%-%ds  %%-%ds  %%-%ds  %%-%ds  %%-%ds", nameW, portsW, pidW, ageW, statusW)
	header := m.styles.listHeader.Render(fmt.Sprintf(headerFmt, "service", "ports", "pid", "uptime", "status"))

	rows := make([]string, 0, len(m.order)+1)
	rows = append(rows, header)
	for i, key := range m.order {
		state := m.services[key]
		if state == nil {
			continue
		}

		cursor := "  "
		if i == m.selected {
			cursor = m.styles.selectedAccent.Render("▌ ")
		}
		rowFmt := fmt.Sprintf("%%s%%-%ds  %%-%ds  %%-%ds  %%-%ds  ", nameW, portsW, pidW, ageW)
		body := fmt.Sprintf(
			rowFmt,
			cursor,
			truncateText(state.config.Name, nameW),
			truncateText(strings.Join(state.config.Ports, ","), portsW),
			pidLabel(state.pid),
			m.uptimeText(state),
		)
		row := body + m.statusChip(state, statusW)

		if i == m.selected {
			row = m.styles.listSelected.Width(width).Render(row)
		} else {
			row = lipgloss.NewStyle().Width(width).Render(row)
		}
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}

func (m model) statusChip(state *serviceState, width int) string {
	glyph := "◯"
	label := "down"
	style := m.styles.chipDown
	switch {
	case state.restarting:
		glyph = spinnerGlyph(m.spinnerFrame)
		label = "restart"
		style = m.styles.chipWarn
	case state.running:
		glyph = "●"
		label = "live"
		style = m.styles.chipUp
	case state.exitErr != nil:
		glyph = "✕"
		label = "exit"
		style = m.styles.chipDanger
	case state.composeDown:
		glyph = "◯"
		label = "down"
		style = m.styles.chipDanger
	}
	rendered := style.Render(glyph + " " + label)
	if pad := width - lipgloss.Width(rendered); pad > 0 {
		rendered += strings.Repeat(" ", pad)
	}
	return rendered
}

// renderFocusLine renders a single line summarizing the selected service's command
// and log file, plus its last-exit message if it has one. Indented to match the
// service list (3-column gutter), and prefixed with a subtle separator above it.
func (m model) renderFocusLine(state *serviceState, width int) string {
	if state == nil {
		return ""
	}
	cmd := state.config.commandText()
	logRel := state.config.LogFile
	if rel, err := filepath.Rel(m.rootDir, logRel); err == nil {
		logRel = rel
	}

	cmdLabel := m.styles.muted.Render("cmd")
	logLabel := m.styles.muted.Render("log")
	sep := m.styles.muted.Render("  ·  ")

	// Reserve space for "  cmd " + " " + "  · " + " log " + log + a small margin.
	cmdMax := max(20, width-len(logRel)-18)
	cmdText := truncateText(cmd, cmdMax)
	line := "  " + cmdLabel + " " + cmdText + sep + logLabel + " " + logRel

	if state.exitErr != nil {
		errMsg := truncateText(state.exitErr.Error(), max(20, width-6))
		line = "  " + m.styles.statusDanger.Render(" "+errMsg+" ")
	}
	return lipgloss.NewStyle().Width(width).Render(line)
}

func (m model) renderLogPanel(state *serviceState, width, height int) string {
	logContent := state.lastLogTail
	if strings.TrimSpace(logContent) == "" {
		logContent = "No output yet. Waiting for the process to write to its log file."
	}
	innerW := max(8, width-4)
	innerH := max(2, height-2)
	logContent = clampLogTail(logContent, innerW, innerH)
	return m.styles.logBox.Width(width - 2).Height(height - 1).Render(logContent)
}

func (m model) renderFooter(width int) string {
	keys := m.styles.muted.Render("j/k move · g/G jump · r restart · q quit")
	right := m.styles.muted.Render(m.logDir)
	gap := width - lipgloss.Width(keys) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return keys + strings.Repeat(" ", gap) + right
}

func (m model) selectedState() *serviceState {
	if len(m.order) == 0 {
		return &serviceState{config: serviceConfig{Name: "unknown"}}
	}
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= len(m.order) {
		m.selected = len(m.order) - 1
	}
	return m.services[m.order[m.selected]]
}

func (m *model) refreshSelectedLog() {
	if len(m.order) == 0 {
		return
	}
	state := m.services[m.order[m.selected]]
	if state == nil {
		return
	}
	if state.config.isComposeService() {
		_ = refreshComposeService(state)
		return
	}
	_ = capLogFile(state.config.LogFile, m.maxLogBytes)
	state.lastLogTail = readLogTail(state.config.LogFile, 18)
}

func (m *model) restartSelectedService() error {
	state := m.selectedState()
	if state == nil {
		return fmt.Errorf("no service selected")
	}

	oldPID := state.pid
	state.restarting = true
	state.exitErr = nil
	state.stoppedAt = time.Time{}
	if state.config.isComposeService() {
		err := restartComposeServices(state.config)
		state.restarting = false
		if err != nil {
			state.running = false
			state.exitErr = err
			state.stoppedAt = time.Now()
			state.lastLogTail = err.Error()
			return err
		}
		state.startedAt = time.Now()
		return refreshComposeService(state)
	}
	if oldPID > 0 {
		if state.ignoredExits == nil {
			state.ignoredExits = make(map[int]struct{})
		}
		state.ignoredExits[oldPID] = struct{}{}
		if err := stopProcessGroup(oldPID); err != nil {
			state.restarting = false
			return err
		}
	}

	nextState, err := startService(state.config, m.exitCh)
	if err != nil {
		state.running = false
		state.pid = 0
		state.cmd = nil
		state.restarting = false
		state.exitErr = err
		state.stoppedAt = time.Now()
		return err
	}

	state.cmd = nextState.cmd
	state.pid = nextState.pid
	state.running = true
	state.restarting = false
	state.startedAt = nextState.startedAt
	state.stoppedAt = time.Time{}
	state.lastLogTail = readLogTail(state.config.LogFile, 18)
	state.ignoredExits = nextState.ignoredExits
	state.restartAttempt = 0
	state.nextRestartAt = time.Time{}
	return nil
}

// autoRestartService respawns a crashed AutoRestart-enabled service in place.
// Backoff state on `state` is updated so a permanently-broken process doesn't hot-loop.
func (m *model) autoRestartService(state *serviceState) error {
	state.restarting = true
	defer func() { state.restarting = false }()

	nextState, err := startService(state.config, m.exitCh)
	if err != nil {
		state.nextRestartAt = time.Now().Add(backoffDelay(state.restartAttempt + 1))
		state.restartAttempt++
		return err
	}

	state.cmd = nextState.cmd
	state.pid = nextState.pid
	state.running = true
	state.exitErr = nil
	state.startedAt = nextState.startedAt
	state.stoppedAt = time.Time{}
	state.lastLogTail = readLogTail(state.config.LogFile, 18)
	state.ignoredExits = nextState.ignoredExits
	// Don't reset restartAttempt yet — the new process may also crash. Reset on
	// the first tick after it stays alive longer than the current backoff.
	state.nextRestartAt = time.Time{}
	return nil
}

// backoffDelay returns an exponential backoff capped at 30s.
// Used by AutoRestart services to avoid hot-looping a permanently-broken process.
func backoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(1<<uint(min(attempt-1, 5))) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func (m model) uptimeText(state *serviceState) string {
	if state == nil {
		return "-"
	}
	if state.running {
		return formatDuration(time.Since(state.startedAt))
	}
	if !state.stoppedAt.IsZero() && !state.startedAt.IsZero() {
		return formatDuration(state.stoppedAt.Sub(state.startedAt))
	}
	return "-"
}

// formatDuration renders a duration compactly with a space between units:
//
//	<1m         → "12s"
//	<1h         → "3m" or "3m 12s" (skip seconds once > 5 minutes)
//	<24h        → "1h" or "1h 47m" (skip minutes once > 6 hours)
//	otherwise   → "2d" or "2d 4h"
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		if m >= 5 || s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		mins := int(d.Minutes()) - h*60
		if h >= 6 || mins == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, mins)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) - days*24
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, hours)
}

func pidLabel(pid int) string {
	if pid <= 0 {
		return "-"
	}
	return strconv.Itoa(pid)
}

func readLogTail(path string, maxLines int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		return "failed to read log: " + err.Error()
	}

	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return ""
	}

	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func clampLogTail(text string, maxWidth, maxLines int) string {
	if strings.TrimSpace(text) == "" {
		return text
	}

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for i := range lines {
		lines[i] = truncateText(lines[i], maxWidth)
	}
	return strings.Join(lines, "\n")
}

func truncateText(value string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxWidth {
		return value
	}
	if maxWidth == 1 {
		return "…"
	}
	return string(runes[:maxWidth-1]) + "…"
}

func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

func newStyles() styles {
	p := palette{
		bg:       lipgloss.Color("#0C0F14"),
		panelAlt: lipgloss.Color("#10161F"),
		border:   lipgloss.Color("#2D3A4F"),
		muted:    lipgloss.Color("#8A95A5"),
		text:     lipgloss.Color("#E7EDF7"),
		accent:   lipgloss.Color("#79E0B3"),
		success:  lipgloss.Color("#83E377"),
		warn:     lipgloss.Color("#FFB86C"),
		danger:   lipgloss.Color("#FF7A90"),
	}

	selectedBg := lipgloss.Color("#1B2638")

	heroBase := lipgloss.NewStyle().
		Background(p.panelAlt).
		Foreground(p.text).
		Border(lipgloss.RoundedBorder()).
		Padding(0, 2).
		MarginBottom(1)

	return styles{
		app: lipgloss.NewStyle().
			Background(p.bg).
			Foreground(p.text).
			Padding(0, 1),
		heroHealthy:  heroBase.BorderForeground(p.accent),
		heroDegraded: heroBase.BorderForeground(p.danger),
		heroTitle: lipgloss.NewStyle().
			Foreground(p.accent).
			Bold(true),
		listHeader: lipgloss.NewStyle().
			Foreground(p.muted).
			Background(p.panelAlt),
		listSelected: lipgloss.NewStyle().
			Background(selectedBg).
			Foreground(p.text).
			Bold(true),
		selectedAccent: lipgloss.NewStyle().
			Foreground(p.accent).
			Background(selectedBg).
			Bold(true),
		kpiValue: lipgloss.NewStyle().
			Foreground(p.text).
			Bold(true),
		muted: lipgloss.NewStyle().
			Foreground(p.muted),
		chipUp: lipgloss.NewStyle().
			Foreground(p.success).
			Bold(true),
		chipWarn: lipgloss.NewStyle().
			Foreground(p.warn).
			Bold(true),
		chipDown: lipgloss.NewStyle().
			Foreground(p.muted),
		chipDanger: lipgloss.NewStyle().
			Foreground(p.danger).
			Bold(true),
		badgeHealthy: lipgloss.NewStyle().
			Foreground(p.bg).
			Background(p.success).
			Bold(true).
			Padding(0, 1),
		badgeWarn: lipgloss.NewStyle().
			Foreground(p.bg).
			Background(p.warn).
			Bold(true).
			Padding(0, 1),
		badgeDanger: lipgloss.NewStyle().
			Foreground(p.text).
			Background(p.danger).
			Bold(true).
			Padding(0, 1),
		statusDanger: lipgloss.NewStyle().
			Foreground(p.text).
			Background(p.danger).
			Bold(true).
			Padding(0, 1),
		logBox: lipgloss.NewStyle().
			Foreground(p.text).
			Background(lipgloss.Color("#0A0E14")).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(p.border).
			Padding(0, 1),
	}
}

func capLogFile(path string, maxBytes int64) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Size() <= maxBytes {
		return nil
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	start := info.Size() - maxBytes
	if start < 0 {
		start = 0
	}

	if _, err := file.Seek(start, 0); err != nil {
		return err
	}

	buf := bytes.NewBuffer(make([]byte, 0, maxBytes))
	if _, err := buf.ReadFrom(file); err != nil {
		return err
	}

	return os.WriteFile(path, buf.Bytes(), 0o644)
}
