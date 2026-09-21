package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type serviceConfig struct {
	Key             string
	Name            string
	DependsOn       []string
	Ports           []string
	WorkDir         string
	LogFile         string
	Command         []string
	Env             map[string]string
	portPlan        *portPlan
	ComposeFile     string
	ComposeServices []string
	ComposeCommand  []string
	// AutoRestart enables exponential-backoff restart of crashed processes.
	AutoRestart bool
	// ManualStart excludes this service from boot unless a dependency needs it.
	ManualStart bool
	// ReadinessTimeout bounds how long startup waits for the service to become ready.
	ReadinessTimeout time.Duration
	// ReadyURL is authoritative when configured; a listening port alone is not readiness.
	ReadyURL string
	LiveURL  string
	registry *processRegistry
}

type serviceState struct {
	config         serviceConfig
	process        *serviceProcess
	pid            int
	running        bool
	starting       bool
	inactive       bool
	startPending   bool
	restarting     bool
	generation     uint64
	exitErr        error
	startedAt      time.Time
	stoppedAt      time.Time
	lastLogTail    string
	restartAttempt int
	nextRestartAt  time.Time
	composeDown    bool
	liveFailures   int
	stopping       bool
	shutdownDone   bool
	keptRunning    bool
	shutdownErr    error
}

type serviceGeneration struct {
	key        string
	generation uint64
}

type processExitMsg struct {
	key        string
	pid        int
	generation uint64
	err        error
}

type shutdownMsg struct{}

type shutdownReadyMsg struct {
	pendingErr error
}

type shutdownProgressMsg struct {
	key   string
	phase string
	err   error
	done  bool
}

type shutdownQuitMsg struct{}

type serviceHealthResult struct {
	key        string
	generation uint64
	running    bool
	summary    string
	err        error
}

type healthCheckMsg struct {
	results []serviceHealthResult
}

type restartResultMsg struct {
	key        string
	generation uint64
	next       *serviceState
	err        error
	oldStopped bool
}

type stopResultMsg struct {
	key        string
	pid        int
	generation uint64
	err        error
}

type serviceBootNode struct {
	done  chan struct{}
	state *serviceState
	err   error
}

type serviceBootResultMsg struct {
	key   string
	state *serviceState
	err   error
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

var composeProjectLocks sync.Map

const (
	composeCommandTimeout = 15 * time.Second
	composeStatusTimeout  = 5 * time.Second
)

type composeProjectLock struct {
	permit chan struct{}
}

func newComposeProjectLock() *composeProjectLock {
	lock := &composeProjectLock{permit: make(chan struct{}, 1)}
	lock.permit <- struct{}{}
	return lock
}

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
	rootDir       string
	logDir        string
	title         string
	maxLogBytes   int64
	services      map[string]*serviceState
	order         []string
	exitCh        chan processExitMsg
	shuttingDown  bool
	anyExited     bool
	selected      int
	lastWheelAt   time.Time
	lastWheel     tea.MouseButton
	selectingText bool
	selectionView string
	width         int
	height        int
	styles        styles
	spinnerFrame  int
	runtimeCtx    context.Context
	cancel        context.CancelFunc
	operations    *operationTracker
	healthBusy    bool
	earlyExits    map[serviceGeneration]error
	interrupted   bool
	bootResults   <-chan serviceBootResultMsg
	bootRemaining int
	bootDone      <-chan struct{}
	shutdownReady bool
	shutdownStart bool
	shutdownPhase string
	shutdownErr   error
	shutdownCh    <-chan shutdownProgressMsg
}

var version = "dev"

func main() {
	if len(os.Args) >= 4 && os.Args[1] == serviceWrapperArg {
		os.Exit(runServiceWrapper(os.Args[3:]))
	}

	showVersion := flag.Bool("version", false, "Print the stack version and exit")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: stack [--version] [--help]")
		fmt.Fprintln(flag.CommandLine.Output(), "\nRun from a repository with stack.toml, or set STACK_DEFAULT_DIR.")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Printf("stack %s\n", version)
		return
	}
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}

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

	registry, err := openProcessRegistry(filepath.Join(cfg.logDir, "processes.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open process registry: %v\n", err)
		os.Exit(1)
	}
	warnings, err := registry.reapStale(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to recover stale processes: %v\n", err)
		os.Exit(1)
	}
	for _, warning := range warnings {
		fmt.Fprintf(os.Stderr, "stack recovery warning: %s\n", warning)
	}

	if err := prepareLogs(cfg.services); err != nil {
		fmt.Fprintf(os.Stderr, "failed to prepare logs: %v\n", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	exitCh := make(chan processExitMsg, len(cfg.services)*2)
	for i := range cfg.services {
		cfg.services[i].ComposeCommand = cfg.composeCommand
		cfg.services[i].registry = registry
	}
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	operations := newOperationTracker()
	services, order, bootResults, bootDone := beginBootServices(runtimeCtx, operations, cfg.services, exitCh, maxLogBytes)

	m := model{
		rootDir:       cfg.rootDir,
		logDir:        cfg.logDir,
		title:         cfg.title,
		maxLogBytes:   maxLogBytes,
		services:      services,
		order:         order,
		exitCh:        exitCh,
		styles:        newStyles(),
		anyExited:     false,
		runtimeCtx:    runtimeCtx,
		cancel:        cancelRuntime,
		operations:    operations,
		earlyExits:    make(map[serviceGeneration]error),
		bootResults:   bootResults,
		bootRemaining: len(cfg.services),
		bootDone:      bootDone,
	}

	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	programDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-sigCh:
				program.Send(shutdownMsg{})
			case <-programDone:
				return
			}
		}
	}()

	finalModel, err := program.Run()
	close(programDone)
	cancelRuntime()
	<-bootDone
	pendingErr := operations.waitAndStopPending()
	var visibleShutdownErr error
	if fm, ok := finalModel.(model); ok {
		visibleShutdownErr = fm.shutdownErr
	}
	shutdownErr := errors.Join(shutdownServices(services, order), pendingErr, visibleShutdownErr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stack terminal failed: %v\n", err)
		os.Exit(1)
	}
	if shutdownErr != nil {
		fmt.Fprintf(os.Stderr, "failed to stop services cleanly: %v\n", shutdownErr)
		os.Exit(1)
	}

	if fm, ok := finalModel.(model); ok && fm.anyExited {
		if fm.interrupted {
			os.Exit(130)
		}
		os.Exit(1)
	} else if ok && fm.interrupted {
		os.Exit(130)
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

	for {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			// A pre-flock stack binary may still own the PID file. Honor that
			// legacy owner during the transition instead of silently taking over.
			data, _ := os.ReadFile(lockPath)
			existingPID, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if existingPID > 0 && existingPID != os.Getpid() && isLiveStackProcess(existingPID) {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
				if !promptTakeover(existingPID) {
					return nil, fmt.Errorf("another stack is already running (pid %d)", existingPID)
				}
				if err := terminateAndWait(existingPID, 10*time.Second); err != nil {
					return nil, fmt.Errorf("failed to stop existing stack (pid %d): %w", existingPID, err)
				}
				continue
			}
			if err := file.Truncate(0); err != nil {
				_ = file.Close()
				return nil, err
			}
			if _, err := file.Seek(0, 0); err != nil {
				_ = file.Close()
				return nil, err
			}
			if _, err := file.WriteString(strconv.Itoa(os.Getpid())); err != nil {
				_ = file.Close()
				return nil, err
			}
			if err := file.Sync(); err != nil {
				_ = file.Close()
				return nil, err
			}

			var once sync.Once
			return func() {
				once.Do(func() {
					// Keep the lock inode stable. Unlinking a flock file allows a
					// waiter on the old inode and a newcomer on the new inode to both
					// become owners.
					_ = file.Truncate(0)
					_ = file.Sync()
					_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
					_ = file.Close()
				})
			}, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		_ = file.Close()

		// Give a just-started owner a brief window to publish its PID.
		var pid int
		for range 10 {
			data, readErr := os.ReadFile(lockPath)
			if readErr == nil {
				pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			}
			if pid > 0 {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if pid <= 0 || !isLiveStackProcess(pid) {
			return nil, fmt.Errorf("another process holds %s", lockPath)
		}
		if !promptTakeover(pid) {
			return nil, fmt.Errorf("another stack is already running (pid %d)", pid)
		}
		if err := terminateAndWait(pid, 10*time.Second); err != nil {
			return nil, fmt.Errorf("failed to stop existing stack (pid %d): %w", pid, err)
		}
	}
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
		return 1024 * 1024, nil
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

func prepareLogs(configs []serviceConfig) error {
	for _, cfg := range configs {
		previous := cfg.LogFile + ".previous"
		if info, err := os.Stat(cfg.LogFile); err == nil && info.Size() > 0 {
			if info.IsDir() {
				return fmt.Errorf("prepare %s log: %s is a directory", cfg.Key, cfg.LogFile)
			}
			if err := os.Remove(previous); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove previous %s log: %w", cfg.Key, err)
			}
			if err := os.Rename(cfg.LogFile, previous); err != nil {
				return fmt.Errorf("rotate %s log: %w", cfg.Key, err)
			}
			if err := os.Chmod(previous, 0o600); err != nil {
				return fmt.Errorf("secure previous %s log: %w", cfg.Key, err)
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s log: %w", cfg.Key, err)
		}
		if err := os.WriteFile(cfg.LogFile, nil, 0o600); err != nil {
			return fmt.Errorf("create %s log: %w", cfg.Key, err)
		}
		if err := os.Chmod(cfg.LogFile, 0o600); err != nil {
			return fmt.Errorf("secure %s log: %w", cfg.Key, err)
		}
	}
	return nil
}

// beginBootServices starts dependency-ready services in the background and
// returns immediately so the dashboard can render boot progress.
func beginBootServices(ctx context.Context, tracker *operationTracker, configs []serviceConfig, exitCh chan<- processExitMsg, maxLogBytes int64) (map[string]*serviceState, []string, <-chan serviceBootResultMsg, <-chan struct{}) {
	configs = dependencyOrderedConfigs(configs)
	wanted := startupServices(configs)
	plan := newPortPlan(configs)
	for i := range configs {
		configs[i].portPlan = plan
	}
	nodes := make(map[string]*serviceBootNode, len(configs))
	services := make(map[string]*serviceState, len(configs))
	order := make([]string, 0, len(configs))
	for _, cfg := range configs {
		nodes[cfg.Key] = &serviceBootNode{done: make(chan struct{})}
		services[cfg.Key] = &serviceState{config: cfg, starting: wanted[cfg.Key], inactive: !wanted[cfg.Key]}
		order = append(order, cfg.Key)
	}

	type bootGroup struct {
		configs []serviceConfig
	}
	groups := make(map[string]*bootGroup)
	for _, cfg := range configs {
		if !wanted[cfg.Key] {
			continue
		}
		key := "process\x00" + cfg.Key
		if cfg.isComposeService() {
			key = "compose\x00" + composeProjectKey(cfg) + "\x00" + strings.Join(cfg.DependsOn, "\x00")
		}
		group := groups[key]
		if group == nil {
			group = &bootGroup{}
			groups[key] = group
		}
		group.configs = append(group.configs, cfg)
	}

	results := make(chan serviceBootResultMsg, len(configs))
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(len(configs))
	finish := func(cfg serviceConfig, state *serviceState, err error) {
		state.starting = false
		state.generation = 1
		tracker.track(state)
		node := nodes[cfg.Key]
		node.state = state
		node.err = err
		close(node.done)
		results <- serviceBootResultMsg{key: cfg.Key, state: state, err: err}
		wg.Done()
	}
	waitDependencies := func(cfg serviceConfig) error {
		for _, dependency := range cfg.DependsOn {
			dependencyNode := nodes[dependency]
			select {
			case <-dependencyNode.done:
			case <-ctx.Done():
				return ctx.Err()
			}
			if dependencyNode.err != nil {
				return fmt.Errorf("dependency %s failed: %w", dependency, dependencyNode.err)
			}
		}
		return nil
	}
	// Inactive rows still emit a result so boot completion includes the catalog.
	for _, cfg := range configs {
		if !wanted[cfg.Key] {
			finish(cfg, &serviceState{config: cfg, inactive: true}, nil)
		}
	}

	for _, group := range groups {
		group := group
		go func() {
			first := group.configs[0]
			if err := waitDependencies(first); err != nil {
				for _, cfg := range group.configs {
					finish(cfg, failedServiceState(cfg, err), err)
				}
				return
			}

			if first.isComposeService() && len(group.configs) > 1 {
				aggregate := first
				aggregate.ComposeServices = nil
				seen := make(map[string]struct{})
				for _, cfg := range group.configs {
					for _, service := range cfg.ComposeServices {
						if _, ok := seen[service]; !ok {
							seen[service] = struct{}{}
							aggregate.ComposeServices = append(aggregate.ComposeServices, service)
						}
					}
				}
				if _, err := startComposeServiceContext(ctx, aggregate); err != nil {
					err = fmt.Errorf("start: %w", err)
					for _, cfg := range group.configs {
						finish(cfg, failedServiceState(cfg, err), err)
					}
					return
				}
				var members sync.WaitGroup
				members.Add(len(group.configs))
				for _, cfg := range group.configs {
					cfg := cfg
					go func() {
						defer members.Done()
						state := &serviceState{config: cfg, running: true, startedAt: time.Now(), lastLogTail: "Compose services are running."}
						state, err := finishBootService(ctx, cfg, state)
						finish(cfg, state, err)
					}()
				}
				members.Wait()
				return
			}

			state, err := bootService(ctx, first, exitCh, maxLogBytes)
			finish(first, state, err)
		}()
	}
	go func() {
		wg.Wait()
		close(results)
		close(done)
	}()
	return services, order, results, done
}

func dependencyOrderedConfigs(configs []serviceConfig) []serviceConfig {
	byKey := make(map[string]serviceConfig, len(configs))
	for _, cfg := range configs {
		byKey[cfg.Key] = cfg
	}
	ordered := make([]serviceConfig, 0, len(configs))
	visited := make(map[string]bool, len(configs))
	var visit func(string)
	visit = func(key string) {
		if visited[key] {
			return
		}
		visited[key] = true
		cfg := byKey[key]
		for _, dependency := range cfg.DependsOn {
			visit(dependency)
		}
		ordered = append(ordered, cfg)
	}
	for _, cfg := range configs {
		visit(cfg.Key)
	}
	return ordered
}

// bootServices is the synchronous test/compatibility wrapper around the
// production streaming boot path.
func bootServices(ctx context.Context, configs []serviceConfig, exitCh chan<- processExitMsg, maxLogBytes int64) (map[string]*serviceState, []string, bool) {
	services, order, results, done := beginBootServices(ctx, nil, configs, exitCh, maxLogBytes)
	failed := false
	for result := range results {
		services[result.key] = result.state
		failed = failed || result.err != nil
	}
	<-done
	return services, order, failed
}

func bootService(ctx context.Context, cfg serviceConfig, exitCh chan<- processExitMsg, maxLogBytes int64) (*serviceState, error) {
	if err := ctx.Err(); err != nil {
		return failedServiceState(cfg, err), err
	}
	state, err := startServiceContext(ctx, cfg, exitCh, 1, maxLogBytes)
	if err != nil {
		return failedServiceState(cfg, fmt.Errorf("start: %w", err)), fmt.Errorf("start: %w", err)
	}

	return finishBootService(ctx, cfg, state)
}

func finishBootService(ctx context.Context, cfg serviceConfig, state *serviceState) (*serviceState, error) {
	cfg = state.config
	if !cfg.isComposeService() {
		state.lastLogTail = readLogTail(cfg.LogFile, 18)
	}
	if err := waitForReadinessContext(ctx, cfg); err != nil {
		bootErr := fmt.Errorf("readiness: %w", err)
		if !cfg.isComposeService() {
			if stopErr := state.process.stop(ctx); stopErr != nil {
				bootErr = errors.Join(bootErr, stopErr)
				state.running = false
				state.exitErr = bootErr
				state.stoppedAt = time.Now()
				state.lastLogTail = readLogTail(cfg.LogFile, 18)
				return state, bootErr
			}
		}
		failed := failedServiceState(cfg, bootErr)
		failed.lastLogTail = readLogTail(cfg.LogFile, 18)
		if failed.lastLogTail == "" {
			failed.lastLogTail = bootErr.Error()
		}
		return failed, bootErr
	}
	return state, nil
}

func failedServiceState(cfg serviceConfig, err error) *serviceState {
	return &serviceState{
		config:      cfg,
		exitErr:     err,
		stoppedAt:   time.Now(),
		lastLogTail: err.Error(),
	}
}

func startServiceContext(ctx context.Context, cfg serviceConfig, exitCh chan<- processExitMsg, generation uint64, maxLogBytes int64) (*serviceState, error) {
	if cfg.isComposeService() {
		return startComposeServiceContext(ctx, cfg)
	}

	if cfg.portPlan == nil {
		cfg.portPlan = newPortPlan([]serviceConfig{cfg})
	}
	resolved, err := cfg.portPlan.prepare(ctx, cfg.Key)
	if err != nil {
		return nil, err
	}
	cfg = resolved

	logFile, err := openCappedLogWriter(cfg.LogFile, maxLogBytes)
	if err != nil {
		return nil, err
	}

	for i, port := range cfg.Ports {
		preferred := cfg.portPlan.originals[cfg.Key].Ports[i]
		if port != preferred {
			_, _ = fmt.Fprintf(logFile, "[stack] port %s occupied; using %s\n", preferred, port)
		}
	}

	ownerToken, err := newProcessOwnerToken()
	if err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("create process ownership token: %w", err)
	}
	command := append([]string(nil), cfg.Command...)
	if cfg.registry != nil {
		executable, err := os.Executable()
		if err != nil {
			_ = logFile.Close()
			return nil, fmt.Errorf("resolve stack executable: %w", err)
		}
		command = append([]string{executable, serviceWrapperArg, ownerToken}, command...)
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = serviceEnvironment(cfg)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = processPipeDrainTimeout

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	if cfg.registry != nil {
		record := processRecord{
			Key:       cfg.Key,
			PID:       cmd.Process.Pid,
			PGID:      cmd.Process.Pid,
			Token:     ownerToken,
			Command:   append([]string(nil), cfg.Command...),
			WorkDir:   cfg.WorkDir,
			LogFile:   cfg.LogFile,
			StartedAt: time.Now(),
		}
		if err := cfg.registry.register(record); err != nil {
			_ = stopProcessGroup(cmd.Process.Pid)
			_ = cmd.Wait()
			_ = logFile.Close()
			return nil, fmt.Errorf("record process ownership: %w", err)
		}
	}
	_, _ = fmt.Fprintf(logFile, "[stack] started pid=%d generation=%d at=%s\n", cmd.Process.Pid, generation, time.Now().Format(time.RFC3339Nano))

	process := &serviceProcess{pid: cmd.Process.Pid}
	state := &serviceState{
		config:     cfg,
		process:    process,
		pid:        cmd.Process.Pid,
		running:    true,
		startedAt:  time.Now(),
		generation: generation,
	}

	go func(key, token string, child *exec.Cmd, file *cappedLogWriter, generation uint64) {
		err := child.Wait()
		// WaitDelay guarantees inherited output pipes cannot postpone cleanup.
		cleanupErr := process.stop(context.Background())
		err = errors.Join(err, cleanupErr)
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		_ = appendSupervisorEvent(cfg.LogFile, "process exited pid=%d generation=%d error=%q", child.Process.Pid, generation, errorText(err))
		if cfg.registry != nil {
			if cleanupErr == nil {
				cleanupErr = cfg.registry.removeIfMatch(key, child.Process.Pid, token)
			}
			if cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
				_ = appendSupervisorEvent(cfg.LogFile, "process cleanup failed pid=%d generation=%d error=%q", child.Process.Pid, generation, cleanupErr.Error())
			}
		}
		exitCh <- processExitMsg{key: key, pid: child.Process.Pid, generation: generation, err: err}
	}(cfg.Key, ownerToken, cmd, logFile, generation)

	return state, nil
}

// runServiceWrapper keeps a stack-owned, token-identifiable process as the
// group leader while the configured command runs as its child. Both receive
// group signals, and the wrapper mirrors the command's exit status.
func runServiceWrapper(command []string) int {
	if len(command) == 0 {
		fmt.Fprintln(os.Stderr, "stack service wrapper: missing command")
		return 127
	}
	child := exec.Command(command[0], command[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "stack service wrapper: %v\n", err)
		return 127
	}
	return 0
}

func startComposeServiceContext(ctx context.Context, cfg serviceConfig) (*serviceState, error) {
	unlock, err := lockComposeProjectContext(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := ensureComposeServicesUpContext(ctx, cfg); err != nil {
		return nil, err
	}

	return &serviceState{
		config:      cfg,
		running:     true,
		startedAt:   time.Now(),
		lastLogTail: "Compose services are running.",
	}, nil
}

// shutdownServices stops independent consumers concurrently, then proceeds
// toward their providers one dependency layer at a time. Compose services are
// intentionally left running across dashboard restarts.
func shutdownServices(services map[string]*serviceState, order []string) error {
	var shutdownErr error
	for progress := range beginShutdownServices(services, order, nil) {
		if progress.done {
			shutdownErr = progress.err
		}
	}
	return shutdownErr
}

// beginShutdownServices streams lifecycle events while stopping a frozen
// service snapshot. The unbuffered channel deliberately gives Bubble Tea a
// render opportunity between transitions instead of racing straight to done.
func beginShutdownServices(services map[string]*serviceState, order []string, initialErr error) <-chan shutdownProgressMsg {
	progress := make(chan shutdownProgressMsg)
	go func() {
		defer close(progress)
		active := make(map[string]bool)
		for _, layer := range shutdownServiceLayers(services, order) {
			for _, key := range layer {
				active[key] = true
			}
		}
		for _, key := range order {
			state := services[key]
			if state == nil || state.inactive {
				continue
			}
			if state.config.isComposeService() {
				progress <- shutdownProgressMsg{key: key, phase: "kept"}
			} else if !active[key] {
				progress <- shutdownProgressMsg{key: key, phase: "stopped"}
			}
		}

		var errs []error
		if initialErr != nil {
			errs = append(errs, initialErr)
		}
		for _, layer := range shutdownServiceLayers(services, order) {
			for _, key := range layer {
				progress <- shutdownProgressMsg{key: key, phase: "stopping"}
			}
			type result struct {
				key string
				err error
			}
			results := make(chan result, len(layer))
			for _, key := range layer {
				state := services[key]
				go func(key string, state *serviceState) {
					err := state.process.stop(context.Background())
					if err != nil {
						err = fmt.Errorf("%s: %w", state.config.Name, err)
					}
					results <- result{key: key, err: err}
				}(key, state)
			}
			for range layer {
				result := <-results
				if result.err != nil {
					errs = append(errs, result.err)
				}
				progress <- shutdownProgressMsg{key: result.key, phase: "stopped", err: result.err}
			}
		}
		progress <- shutdownProgressMsg{done: true, err: errors.Join(errs...)}
	}()
	return progress
}

func cloneServiceStates(services map[string]*serviceState) map[string]*serviceState {
	cloned := make(map[string]*serviceState, len(services))
	for key, state := range services {
		if state == nil {
			continue
		}
		copy := *state
		cloned[key] = &copy
	}
	return cloned
}

func prepareShutdownCmd(bootDone <-chan struct{}, operations *operationTracker) tea.Cmd {
	return func() tea.Msg {
		if bootDone != nil {
			<-bootDone
		}
		var pendingErr error
		if operations != nil {
			pendingErr = operations.waitAndStopPending()
		}
		return shutdownReadyMsg{pendingErr: pendingErr}
	}
}

func waitForShutdownProgressCmd(ch <-chan shutdownProgressMsg) tea.Cmd {
	return func() tea.Msg {
		progress, ok := <-ch
		if !ok {
			return shutdownProgressMsg{done: true, err: errors.New("shutdown progress channel closed before completion")}
		}
		return progress
	}
}

func shutdownQuitCmd(delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(time.Time) tea.Msg { return shutdownQuitMsg{} })
}

func shutdownServiceLayers(services map[string]*serviceState, order []string) [][]string {
	pending := make(map[string]*serviceState)
	for _, key := range order {
		state := services[key]
		if state == nil || state.config.isComposeService() || state.process == nil {
			continue
		}
		pending[key] = state
	}
	var layers [][]string
	for len(pending) > 0 {
		dependents := make(map[string]bool)
		for _, state := range pending {
			for _, dependency := range state.config.DependsOn {
				if _, ok := pending[dependency]; ok {
					dependents[dependency] = true
				}
			}
		}
		layer := make([]string, 0)
		for i := len(order) - 1; i >= 0; i-- {
			key := order[i]
			if _, ok := pending[key]; ok && !dependents[key] {
				layer = append(layer, key)
			}
		}
		if len(layer) == 0 {
			break
		}
		for _, key := range layer {
			delete(pending, key)
		}
		layers = append(layers, layer)
	}
	return layers
}

func stopProcessGroup(pid int) error {
	return stopProcessGroupContext(context.Background(), pid)
}

func stopProcessGroupContext(ctx context.Context, pid int) error {
	if pid <= 0 {
		return nil
	}

	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(-pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			waitForPIDReap(pid, 500*time.Millisecond)
			return nil
		}
		if err := waitForPoll(ctx, 100*time.Millisecond); err != nil {
			break
		}
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	killDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(killDeadline) {
		if err := syscall.Kill(-pid, 0); errors.Is(err, syscall.ESRCH) {
			waitForPIDReap(pid, 500*time.Millisecond)
			return nil
		}
		// Cleanup must finish even after cancellation, so use a short bounded
		// background wait after escalating to SIGKILL.
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("process group %d did not exit after SIGKILL", pid)
}

func waitForPIDReap(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForReadiness blocks until the configured health URL returns 200 or every
// declared port accepts a TCP connection. A URL is authoritative when present.
func waitForReadiness(cfg serviceConfig) error {
	return waitForReadinessContext(context.Background(), cfg)
}

func waitForReadinessContext(ctx context.Context, cfg serviceConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.ReadyURL == "" && len(cfg.Ports) == 0 {
		return nil
	}
	timeout := cfg.ReadinessTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if cfg.ReadyURL != "" {
		client := &http.Client{Timeout: 500 * time.Millisecond}
		for time.Now().Before(deadline) {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.ReadyURL, nil)
			if err != nil {
				return err
			}
			response, err := client.Do(request)
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return nil
				}
			}
			if err := waitForPoll(ctx, 250*time.Millisecond); err != nil {
				return err
			}
		}
		return fmt.Errorf("%s did not become ready within %s", cfg.ReadyURL, timeout)
	}
	pending := make(map[string]struct{}, len(cfg.Ports))
	for _, port := range cfg.Ports {
		pending[port] = struct{}{}
	}
	for time.Now().Before(deadline) {
		for port := range pending {
			addr := net.JoinHostPort("127.0.0.1", port)
			dialer := net.Dialer{Timeout: 250 * time.Millisecond}
			conn, err := dialer.DialContext(ctx, "tcp", addr)
			if err == nil {
				_ = conn.Close()
				delete(pending, port)
			}
		}
		if len(pending) == 0 {
			return nil
		}
		if err := waitForPoll(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	ports := make([]string, 0, len(pending))
	for _, port := range cfg.Ports {
		if _, ok := pending[port]; ok {
			ports = append(ports, port)
		}
	}
	return fmt.Errorf("ports %s did not become ready within %s", strings.Join(ports, ", "), timeout)
}

func waitForPoll(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// checkHealthURL accepts only HTTP 200 so a bound but degraded service is not treated as live.
func checkHealthURL(rawURL string) error {
	return checkHealthURLContext(context.Background(), rawURL)
}

func checkHealthURLContext(ctx context.Context, rawURL string) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", rawURL, response.Status)
	}
	return nil
}

func checkServiceHealthOnce(ctx context.Context, cfg serviceConfig) error {
	// Readiness is the startup gate; liveness is the authoritative steady-state
	// signal when one is configured. Falling back to readiness keeps older
	// configs monitored without forcing them to declare both endpoints.
	if cfg.LiveURL != "" {
		return checkHealthURLContext(ctx, cfg.LiveURL)
	}
	if cfg.ReadyURL != "" {
		return checkHealthURLContext(ctx, cfg.ReadyURL)
	}
	for _, port := range cfg.Ports {
		address := net.JoinHostPort("127.0.0.1", port)
		dialer := net.Dialer{Timeout: 250 * time.Millisecond}
		connection, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("port %s: %w", port, err)
		}
		_ = connection.Close()
	}
	return nil
}

func (cfg serviceConfig) isComposeService() bool {
	return len(cfg.ComposeServices) > 0
}

func (cfg serviceConfig) composeBaseArgs() []string {
	return []string{"-f", cfg.ComposeFile}
}

func (cfg serviceConfig) composeCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	command := cfg.ComposeCommand
	if len(command) == 0 {
		command = []string{"podman", "compose"}
	}
	cmd := exec.CommandContext(ctx, command[0], append(command[1:], args...)...)
	cmd.Dir = cfg.WorkDir
	configureHelperProcess(cmd)
	return cmd
}

func composeProjectKey(cfg serviceConfig) string {
	return strings.Join(cfg.ComposeCommand, "\x00") + "\x00" + cfg.WorkDir + "\x00" + cfg.ComposeFile
}

func lockComposeProjectContext(ctx context.Context, cfg serviceConfig) (func(), error) {
	value, _ := composeProjectLocks.LoadOrStore(composeProjectKey(cfg), newComposeProjectLock())
	lock := value.(*composeProjectLock)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lock.permit:
		return func() { lock.permit <- struct{}{} }, nil
	}
}

func (cfg serviceConfig) commandText() string {
	if cfg.isComposeService() {
		return "compose: " + strings.Join(cfg.ComposeServices, " ")
	}
	return strings.Join(cfg.Command, " ")
}

func ensureComposeServicesUpContext(ctx context.Context, cfg serviceConfig) error {
	statuses, err := readComposeStatusesContext(ctx, cfg)
	if err != nil {
		return err
	}
	if composeServicesRunning(cfg, statuses) {
		return nil
	}

	args := append(cfg.composeBaseArgs(), append([]string{"up", "-d"}, cfg.ComposeServices...)...)
	commandCtx, cancel := context.WithTimeout(ctx, composeCommandTimeout)
	cmd := cfg.composeCommandContext(commandCtx, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		cancel()
		return fmt.Errorf("start compose services: %w%s", err, formatCommandOutput(output))
	}
	cancel()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		statuses, err = readComposeStatusesContext(ctx, cfg)
		if err == nil && composeServicesRunning(cfg, statuses) {
			return nil
		}
		if err := waitForPoll(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("compose services did not reach running state within 10s")
}

func restartComposeServicesContext(ctx context.Context, cfg serviceConfig) error {
	unlock, err := lockComposeProjectContext(ctx, cfg)
	if err != nil {
		return err
	}
	defer unlock()
	args := append(cfg.composeBaseArgs(), append([]string{"restart"}, cfg.ComposeServices...)...)
	commandCtx, cancel := context.WithTimeout(ctx, composeCommandTimeout)
	defer cancel()
	cmd := cfg.composeCommandContext(commandCtx, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restart compose services: %w%s", err, formatCommandOutput(output))
	}
	return ensureComposeServicesUpContext(ctx, cfg)
}

func readComposeStatusesContext(ctx context.Context, cfg serviceConfig) ([]composeServiceStatus, error) {
	commandCtx, cancel := context.WithTimeout(ctx, composeStatusTimeout)
	defer cancel()
	args := append(cfg.composeBaseArgs(), append([]string{"ps", "--format", "json"}, cfg.ComposeServices...)...)
	cmd := cfg.composeCommandContext(commandCtx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read compose status: %w%s", err, formatCommandOutput(stderr.Bytes()))
	}

	// Newer Docker Compose emits a JSON array; older versions emit one
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

func healthCheckCmd(ctx context.Context, tracker *operationTracker, services map[string]*serviceState) tea.Cmd {
	type healthSnapshot struct {
		config     serviceConfig
		generation uint64
	}
	configs := make([]healthSnapshot, 0, len(services))
	for _, state := range services {
		if state == nil || state.inactive || state.startPending || state.starting || state.restarting {
			continue
		}
		if !state.config.isComposeService() && !state.running {
			continue
		}
		configs = append(configs, healthSnapshot{config: state.config, generation: state.generation})
	}

	return tracker.command(func() tea.Msg {
		type composeGroup struct {
			config  serviceConfig
			members []healthSnapshot
			seen    map[string]struct{}
		}
		groups := make(map[string]*composeGroup)
		plain := make([]healthSnapshot, 0, len(configs))
		for _, snapshot := range configs {
			cfg := snapshot.config
			if !cfg.isComposeService() {
				plain = append(plain, snapshot)
				continue
			}
			key := composeProjectKey(cfg)
			group := groups[key]
			if group == nil {
				group = &composeGroup{config: cfg, seen: make(map[string]struct{})}
				group.config.ComposeServices = nil
				groups[key] = group
			}
			group.members = append(group.members, snapshot)
			for _, service := range cfg.ComposeServices {
				if _, ok := group.seen[service]; !ok {
					group.seen[service] = struct{}{}
					group.config.ComposeServices = append(group.config.ComposeServices, service)
				}
			}
		}

		resultCh := make(chan serviceHealthResult, len(configs))
		var checks sync.WaitGroup
		for _, snapshot := range plain {
			checks.Add(1)
			go func() {
				defer checks.Done()
				resultCh <- serviceHealthResult{
					key:        snapshot.config.Key,
					generation: snapshot.generation,
					running:    true,
					err:        checkServiceHealthOnce(ctx, snapshot.config),
				}
			}()
		}
		for _, group := range groups {
			checks.Add(1)
			go func() {
				defer checks.Done()
				unlock, lockErr := lockComposeProjectContext(ctx, group.config)
				var statuses []composeServiceStatus
				readErr := lockErr
				if lockErr == nil {
					statuses, readErr = readComposeStatusesContext(ctx, group.config)
					unlock()
				}
				for _, snapshot := range group.members {
					cfg := snapshot.config
					result := serviceHealthResult{
						key:        cfg.Key,
						generation: snapshot.generation,
						running:    readErr == nil && composeServicesRunning(cfg, statuses),
						summary:    composeStatusSummary(cfg, statuses),
						err:        readErr,
					}
					if result.err == nil && !result.running {
						result.err = fmt.Errorf("one or more compose services are not running")
					}
					if result.err == nil {
						result.err = checkServiceHealthOnce(ctx, cfg)
					}
					resultCh <- result
				}
			}()
		}
		checks.Wait()
		close(resultCh)
		results := make([]serviceHealthResult, 0, len(configs))
		for result := range resultCh {
			results = append(results, result)
		}
		sort.Slice(results, func(i, j int) bool { return results[i].key < results[j].key })
		return healthCheckMsg{results: results}
	})
}

func restartServiceCmd(ctx context.Context, tracker *operationTracker, key string, cfg serviceConfig, previous *serviceProcess, generation uint64, maxLogBytes int64, exitCh chan<- processExitMsg) tea.Cmd {
	return tracker.command(func() tea.Msg {
		result := restartResultMsg{key: key, generation: generation, oldStopped: previous == nil}
		if cfg.isComposeService() {
			if err := restartComposeServicesContext(ctx, cfg); err != nil {
				result.err = err
				return result
			}
			next := &serviceState{
				config:      cfg,
				running:     true,
				startedAt:   time.Now(),
				lastLogTail: "Compose services are running.",
				generation:  generation,
			}
			if err := waitForReadinessContext(ctx, cfg); err != nil {
				next.running = false
				next.exitErr = fmt.Errorf("readiness: %w", err)
				next.stoppedAt = time.Now()
				result.next = next
				result.err = next.exitErr
				return result
			}
			result.next = next
			return result
		}

		if previous != nil {
			if err := previous.stop(ctx); err != nil {
				result.err = err
				return result
			}
			result.oldStopped = true
		}
		if err := ctx.Err(); err != nil {
			result.err = err
			return result
		}

		next, err := startServiceContext(ctx, cfg, exitCh, generation, maxLogBytes)
		if err != nil {
			result.err = err
			return result
		}
		tracker.track(next)
		if err := waitForReadinessContext(ctx, next.config); err != nil {
			stopErr := next.process.stop(ctx)
			if stopErr == nil {
				tracker.release(next.process)
				result.err = err
				return result
			}
			next.running = false
			next.exitErr = errors.Join(err, stopErr)
			next.stoppedAt = time.Now()
			result.next = next
			result.err = next.exitErr
			return result
		}
		result.next = next
		return result
	})
}

func stopServiceCmd(ctx context.Context, tracker *operationTracker, key string, process *serviceProcess, generation uint64) tea.Cmd {
	return tracker.command(func() tea.Msg {
		return stopResultMsg{key: key, pid: process.pid, generation: generation, err: process.stop(ctx)}
	})
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

func waitForBootResultCmd(ch <-chan serviceBootResultMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m model) Init() tea.Cmd {
	commands := []tea.Cmd{tickCmd(), spinnerTickCmd(), waitForExitCmd(m.exitCh)}
	if m.bootRemaining > 0 {
		commands = append(commands, waitForBootResultCmd(m.bootResults))
	}
	return tea.Batch(commands...)
}

// anyAnimating reports whether something on screen wants a fast redraw.
// Avoids burning a 100ms tick when the dashboard is not showing a spinner.
func (m model) anyAnimating() bool {
	for _, s := range m.services {
		if s != nil && (s.startPending || s.starting || s.restarting || s.stopping) {
			return true
		}
	}
	return false
}

func (m model) dependenciesReady(cfg serviceConfig) error {
	for _, key := range cfg.DependsOn {
		dependency := m.services[key]
		if dependency == nil || dependency.starting || !dependency.running || dependency.restarting || dependency.exitErr != nil {
			return fmt.Errorf("dependency %s is not ready", key)
		}
	}
	return nil
}

func (m model) currentlyDegraded() bool {
	for _, state := range m.services {
		if state != nil && (state.inactive || state.startPending || state.starting) {
			continue
		}
		if state == nil || !state.running || state.restarting || state.exitErr != nil {
			return true
		}
	}
	return false
}

func (m model) requestShutdown(interrupted bool) (tea.Model, tea.Cmd) {
	if m.shuttingDown {
		// A second quit request is the escape hatch if terminal teardown itself is
		// more important than watching the remaining cleanup. main retains an
		// idempotent shutdown fallback after Bubble Tea returns.
		return m, tea.Quit
	}
	m.shuttingDown = true
	m.interrupted = interrupted
	m.shutdownPhase = "draining in-flight work"
	if m.cancel != nil {
		m.cancel()
	}
	return m, prepareShutdownCmd(m.bootDone, m.operations)
}

func (m *model) beginVisibleShutdown(pendingErr error) tea.Cmd {
	if m.shutdownStart {
		return nil
	}
	m.shutdownStart = true
	m.shutdownPhase = "stopping services"
	snapshot := cloneServiceStates(m.services)
	m.shutdownCh = beginShutdownServices(snapshot, append([]string(nil), m.order...), pendingErr)
	return waitForShutdownProgressCmd(m.shutdownCh)
}

func (m *model) applyShutdownProgress(msg shutdownProgressMsg) {
	state := m.services[msg.key]
	if state == nil {
		return
	}
	state.startPending = false
	switch msg.phase {
	case "kept":
		state.starting = false
		state.restarting = false
		state.stopping = false
		state.keptRunning = true
	case "stopping":
		state.starting = false
		state.restarting = false
		state.stopping = true
		state.keptRunning = false
	case "stopped":
		state.starting = false
		state.restarting = false
		state.stopping = false
		state.shutdownErr = msg.err
		if msg.err == nil {
			state.running = false
			state.pid = 0
			state.process = nil
			state.shutdownDone = true
			state.stoppedAt = time.Now()
		}
	}
}

func (m *model) applyRestartResult(msg restartResultMsg) {
	state := m.services[msg.key]
	if state == nil {
		return
	}
	if msg.generation <= state.generation {
		return
	}
	preservedAttempt := state.restartAttempt
	state.restarting = false
	state.starting = false

	if msg.next != nil {
		m.operations.release(msg.next.process)
		*state = *msg.next
		state.restartAttempt = preservedAttempt
		if earlyErr, exited := m.earlyExits[serviceGeneration{msg.key, state.generation}]; exited {
			delete(m.earlyExits, serviceGeneration{msg.key, state.generation})
			state.running = false
			if earlyErr == nil {
				earlyErr = errors.New("process exited during restart")
			}
			state.exitErr = earlyErr
			state.retireStoppedProcess()
			state.stoppedAt = time.Now()
			msg.err = earlyErr
		}
	} else {
		delete(m.earlyExits, serviceGeneration{msg.key, msg.generation})
		if msg.oldStopped {
			state.generation = msg.generation
		}
	}
	if msg.next == nil && msg.oldStopped {
		state.process = nil
		state.pid = 0
		state.running = false
		state.stoppedAt = time.Now()
	}

	if msg.err != nil {
		state.exitErr = msg.err
		if msg.next != nil {
			state.running = false
		}
		if state.config.AutoRestart {
			state.restartAttempt++
			state.nextRestartAt = time.Now().Add(backoffDelay(state.restartAttempt))
		}
		return
	}

	state.running = true
	state.exitErr = nil
	state.stoppedAt = time.Time{}
	// Preserve crash history until the healthy-window check resets it.
	state.nextRestartAt = time.Time{}
	state.liveFailures = 0
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	updated, command := m.update(msg)
	next := updated.(model)
	return next.advanceStarts(command)
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case serviceBootResultMsg:
		state := m.services[msg.key]
		if state != nil && state.starting && !msg.state.inactive {
			*state = *msg.state
			m.operations.release(state.process)
			if earlyErr, exited := m.earlyExits[serviceGeneration{msg.key, state.generation}]; exited {
				delete(m.earlyExits, serviceGeneration{msg.key, state.generation})
				if msg.err == nil {
					if earlyErr == nil {
						earlyErr = errors.New("process exited during startup")
					}
					state.running = false
					state.exitErr = earlyErr
					state.retireStoppedProcess()
					state.stoppedAt = time.Now()
				}
			}
		}
		if m.bootRemaining > 0 {
			m.bootRemaining--
		}
		if !m.shuttingDown {
			m.anyExited = m.currentlyDegraded()
		}
		if m.bootRemaining > 0 {
			return m, waitForBootResultCmd(m.bootResults)
		}
		if m.shuttingDown && m.shutdownReady {
			return m, m.beginVisibleShutdown(m.shutdownErr)
		}
		return m, nil
	case shutdownMsg:
		m.selectingText, m.selectionView = false, ""
		return m.requestShutdown(true)
	case shutdownReadyMsg:
		m.shutdownReady = true
		m.shutdownErr = msg.pendingErr
		if m.bootRemaining > 0 {
			m.shutdownPhase = "canceling startup"
			return m, nil
		}
		return m, m.beginVisibleShutdown(msg.pendingErr)
	case shutdownProgressMsg:
		if msg.done {
			m.shutdownErr = msg.err
			if m.shutdownErr != nil {
				m.shutdownPhase = "shutdown errors"
				return m, shutdownQuitCmd(900 * time.Millisecond)
			}
			m.shutdownPhase = "shutdown complete"
			return m, shutdownQuitCmd(400 * time.Millisecond)
		}
		m.applyShutdownProgress(msg)
		return m, waitForShutdownProgressCmd(m.shutdownCh)
	case shutdownQuitMsg:
		return m, tea.Quit
	case tea.KeyMsg:
		if msg.String() == "m" && !m.shuttingDown {
			m.selectingText = !m.selectingText
			m.selectionView = ""
			m.lastWheelAt = time.Time{}
			if m.selectingText {
				// Keep the displayed text still while terminal-native selection is active.
				// Supervision continues through Update in the background.
				m.selectionView = m.View()
				return m, tea.DisableMouse
			}
			return m, tea.EnableMouseCellMotion
		}
		if m.selectingText {
			if msg.String() != "q" && msg.String() != "ctrl+c" {
				return m, nil
			}
			m.selectingText, m.selectionView = false, ""
		}
		if m.shuttingDown && msg.String() != "ctrl+c" && msg.String() != "q" {
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", "q":
			return m.requestShutdown(false)
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
		case "s":
			if state := m.selectedState(); state != nil && !state.running {
				m.requestStart(state.config.Key)
			}
		case "r":
			state := m.selectedState()
			if state == nil || state.startPending || state.starting || state.restarting {
				break
			}
			if state.inactive {
				m.requestStart(state.config.Key)
				break
			}
			if err := m.dependenciesReady(state.config); err != nil {
				state.exitErr = err
				state.lastLogTail = err.Error()
				m.anyExited = m.currentlyDegraded()
				break
			}
			state.restarting = true
			m.anyExited = m.currentlyDegraded()
			return m, restartServiceCmd(m.runtimeCtx, m.operations, state.config.Key, state.config, state.process, state.generation+1, m.maxLogBytes, m.exitCh)
		}
	case tea.MouseMsg:
		m.handleMouse(msg, time.Now())
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.selectingText {
			m.selectionView = ""
			m.selectionView = m.View()
		}
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
		commands := []tea.Cmd{tickCmd()}
		selectedKey := ""
		if selected := m.selectedState(); selected != nil {
			selectedKey = selected.config.Key
		}
		for _, state := range m.services {
			if state.inactive || state.startPending || state.starting {
				continue
			}
			if !state.config.isComposeService() {
				if state.config.Key == selectedKey {
					state.lastLogTail = readLogTail(state.config.LogFile, 18)
				}
				resetRestartBackoffAfterHealthy(state, now)
			}
			if state.running && !state.restarting && !m.shuttingDown && !state.config.isComposeService() && state.config.portPlan != nil && state.config.portPlan.changed(state.config) && m.dependenciesReady(state.config) == nil {
				state.restarting = true
				commands = append(commands, restartServiceCmd(m.runtimeCtx, m.operations, state.config.Key, state.config, state.process, state.generation+1, m.maxLogBytes, m.exitCh))
				continue
			}
			if state.config.AutoRestart && !state.running && !state.restarting && !m.shuttingDown {
				if !state.nextRestartAt.IsZero() && now.Before(state.nextRestartAt) {
					continue
				}
				if m.dependenciesReady(state.config) != nil {
					continue
				}
				state.restarting = true
				commands = append(commands, restartServiceCmd(m.runtimeCtx, m.operations, state.config.Key, state.config, state.process, state.generation+1, m.maxLogBytes, m.exitCh))
			}
		}
		if !m.healthBusy && !m.shuttingDown {
			m.healthBusy = true
			commands = append(commands, healthCheckCmd(m.runtimeCtx, m.operations, m.services))
		}
		if !m.shuttingDown {
			m.anyExited = m.currentlyDegraded()
		}
		return m, tea.Batch(commands...)
	case healthCheckMsg:
		m.healthBusy = false
		if m.shuttingDown {
			return m, nil
		}
		var commands []tea.Cmd
		for _, result := range msg.results {
			state := m.services[result.key]
			if state == nil || state.inactive || state.startPending || state.starting || state.restarting || state.generation != result.generation {
				continue
			}
			if state.config.isComposeService() {
				state.lastLogTail = result.summary
				if result.err != nil {
					state.running = false
					state.exitErr = result.err
					state.stoppedAt = time.Now()
					if state.lastLogTail == "" {
						state.lastLogTail = result.err.Error()
					}
				} else {
					state.running = result.running
					state.exitErr = nil
					state.stoppedAt = time.Time{}
				}
				continue
			}
			if result.err == nil {
				state.liveFailures = 0
				if state.running {
					state.exitErr = nil
				}
				continue
			}
			state.liveFailures++
			if state.liveFailures < 3 || state.pid <= 0 {
				continue
			}
			state.exitErr = fmt.Errorf("liveness failed: %w", result.err)
			state.restarting = true
			commands = append(commands, stopServiceCmd(m.runtimeCtx, m.operations, state.config.Key, state.process, state.generation))
		}
		m.anyExited = m.currentlyDegraded()
		return m, tea.Batch(commands...)
	case restartResultMsg:
		if m.shuttingDown {
			// The tracker owns discarded results until their processes are stopped.
			return m, nil
		}
		m.applyRestartResult(msg)
		m.anyExited = m.currentlyDegraded()
		return m, nil
	case stopResultMsg:
		state := m.services[msg.key]
		if state != nil && state.pid == msg.pid && state.generation == msg.generation {
			state.restarting = false
			if msg.err != nil {
				state.exitErr = errors.Join(state.exitErr, msg.err)
				state.liveFailures = 0
			} else {
				state.running = false
				state.process = nil
				state.pid = 0
				state.stoppedAt = time.Now()
				state.restartAttempt++
				state.nextRestartAt = time.Now().Add(backoffDelay(state.restartAttempt))
			}
		}
		if !m.shuttingDown {
			m.anyExited = m.currentlyDegraded()
		}
		return m, nil
	case processExitMsg:
		state, ok := m.services[msg.key]
		if ok {
			if msg.generation < state.generation || (msg.generation == state.generation && state.restarting) {
				return m, waitForExitCmd(m.exitCh)
			}
			if msg.generation > state.generation {
				if m.earlyExits == nil {
					m.earlyExits = make(map[serviceGeneration]error)
				}
				m.earlyExits[serviceGeneration{msg.key, msg.generation}] = msg.err
				return m, waitForExitCmd(m.exitCh)
			}
			if state.pid != msg.pid {
				return m, waitForExitCmd(m.exitCh)
			}
			state.running = false
			state.restarting = false
			state.retireStoppedProcess()
			state.exitErr = msg.err
			state.stoppedAt = time.Now()
			state.lastLogTail = readLogTail(state.config.LogFile, 18)
			if state.config.AutoRestart && !m.shuttingDown {
				state.restartAttempt++
				state.nextRestartAt = time.Now().Add(backoffDelay(state.restartAttempt))
				m.anyExited = m.currentlyDegraded()
				return m, waitForExitCmd(m.exitCh)
			}
		}
		if !m.shuttingDown {
			m.anyExited = m.currentlyDegraded()
		}
		return m, waitForExitCmd(m.exitCh)
	}

	return m, nil
}

func (m model) View() string {
	if m.selectingText && m.selectionView != "" {
		return m.selectionView
	}
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
		case state.stopping:
			status = spinnerGlyph(m.spinnerFrame) + " stopping"
		case state.shutdownErr != nil:
			status = "✕ failed"
		case state.keptRunning:
			status = "● kept"
		case state.shutdownDone:
			status = "◯ stopped"
		case state.inactive:
			status = "◯ inactive"
		case state.startPending:
			status = spinnerGlyph(m.spinnerFrame) + " waiting"
		case state.starting:
			status = spinnerGlyph(m.spinnerFrame) + " starting"
		case state.restarting:
			status = spinnerGlyph(m.spinnerFrame) + " restart"
		case state.running:
			status = "● live"
		case state.exitErr != nil:
			status = "✕ exit"
		}
		b.WriteString(fmt.Sprintf("%s %-10s %-10s pid=%d ports=%s\n", cursor, state.config.Name, status, state.pid, strings.Join(state.config.Ports, ",")))
	}
	if m.shuttingDown {
		b.WriteString("\n" + m.shutdownPhase + "\n")
	}
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("selected: %s\n", selected.config.LogFile))
	if selected.inactive {
		b.WriteString("Optional service. Press s to start.\n")
	} else if selected.lastLogTail == "" {
		b.WriteString("no log output yet\n")
	} else {
		b.WriteString(selected.lastLogTail)
	}
	if m.selectingText {
		b.WriteString("\nselect text to copy · m resume · q quit\n")
	} else if !m.shuttingDown {
		b.WriteString("\n" + m.actionHint() + " · q quit\n")
	}
	return b.String()
}

func (m model) renderHero(width int) string {
	upCount, activeCount := 0, 0
	for _, key := range m.order {
		state := m.services[key]
		if state != nil && state.inactive {
			continue
		}
		activeCount++
		if state != nil && state.running {
			upCount++
		}
	}

	// Healthy: solid green pill. Degraded: alternate danger/warn each 500ms
	// (5 spinner frames) so the eye is drawn to the count without it being
	// a frenetic flash.
	badgeText := fmt.Sprintf(" %d/%d live ", upCount, activeCount)
	if activeCount == 0 {
		badgeText = " all inactive "
	}
	var badge string
	if m.shuttingDown {
		badgeText = " shutting down "
		badge = m.styles.badgeWarn.Render(badgeText)
	} else if !m.anyExited {
		badge = m.styles.badgeHealthy.Render(badgeText)
	} else if (m.spinnerFrame/5)%2 == 0 {
		badge = m.styles.badgeDanger.Render(badgeText)
	} else {
		badge = m.styles.badgeWarn.Render(badgeText)
	}

	title := m.styles.heroTitle.Render(m.title)

	// Middle: show the currently selected service so the eye has a target.
	var middle string
	if m.shuttingDown {
		middle = m.styles.kpiValue.Render(truncateText(m.shutdownPhase, 24))
	} else if sel := m.selectedState(); sel != nil && sel.config.Name != "" {
		middle = m.styles.muted.Render("focus ") + m.styles.kpiValue.Render(sel.config.Name)
	}

	metaText := fmt.Sprintf("log cap %s", humanBytes(m.maxLogBytes))
	if m.shuttingDown {
		metaText = "compose stays up"
		if width < 100 {
			metaText = ""
		}
	}
	meta := m.styles.muted.Render(metaText)

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
	ageW = 8     // fits "1m 35s" / "2d 4h" with the new space separator
	statusW = 10 // fits the longest chip: "⠋ starting"
	// Guard against absurd port lists eating the entire row.
	maxPorts := max(8, width-nameW-pidW-ageW-statusW-12)
	if portsW > maxPorts {
		portsW = maxPorts
	}
	return
}

func (m model) renderServiceList(width int) string {
	return strings.Join(m.serviceListRows(width), "\n")
}

// Keep hit testing and rendering on the same rows, including wrapped content.
func (m model) serviceListRows(width int) []string {
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
	return rows
}

// serviceAt returns the service under a terminal cell, excluding other panels.
func (m model) serviceAt(x, y int) (int, bool) {
	if x < 0 || x >= m.width || y < 0 || y >= m.height {
		return 0, false
	}
	// Bubble Tea keeps the bottom of a view when it exceeds the terminal height.
	y += max(0, lipgloss.Height(m.View())-m.height)
	if m.width < 60 || m.height < 12 {
		index := y - lipgloss.Height(m.title+"\nresize terminal for dashboard mode\n")
		return index, index >= 0 && index < len(m.order)
	}
	width := max(60, m.width-2)
	left := m.styles.app.GetMarginLeft() + m.styles.app.GetBorderLeftSize() + m.styles.app.GetPaddingLeft()
	if x < left || x >= left+width {
		return 0, false
	}
	top := m.styles.app.GetMarginTop() + m.styles.app.GetBorderTopSize() + m.styles.app.GetPaddingTop()
	rows := m.serviceListRows(width)
	top += lipgloss.Height(m.renderHero(width)) + lipgloss.Height(rows[0])
	row := 1
	for i, key := range m.order {
		if m.services[key] == nil {
			continue
		}
		height := lipgloss.Height(rows[row])
		if y >= top && y < top+height {
			return i, true
		}
		top += height
		row++
	}
	return 0, false
}

const wheelStepInterval = 100 * time.Millisecond

func (m *model) handleMouse(msg tea.MouseMsg, now time.Time) {
	if m.selectingText || m.shuttingDown || len(m.order) == 0 || msg.Action != tea.MouseActionPress {
		return
	}
	switch msg.Button {
	case tea.MouseButtonLeft:
		if index, ok := m.serviceAt(msg.X, msg.Y); ok {
			m.selected = index
			m.lastWheelAt = time.Time{}
			m.refreshSelectedLog()
		}
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		// Terminals and trackpads can emit a burst for one wheel gesture.
		// Limit same-direction repeats, but let a reversal take effect immediately.
		if msg.Button == m.lastWheel && now.Sub(m.lastWheelAt) < wheelStepInterval {
			return
		}
		m.lastWheelAt, m.lastWheel = now, msg.Button
		delta := 1
		if msg.Button == tea.MouseButtonWheelUp {
			delta = -1
		}
		m.selected = max(0, min(len(m.order)-1, m.selected+delta))
		m.refreshSelectedLog()
	}
}

func (m model) statusChip(state *serviceState, width int) string {
	glyph := "◯"
	label := "down"
	style := m.styles.chipDown
	switch {
	case state.stopping:
		glyph = spinnerGlyph(m.spinnerFrame)
		label = "stopping"
		style = m.styles.chipWarn
	case state.shutdownErr != nil:
		glyph = "✕"
		label = "failed"
		style = m.styles.chipDanger
	case state.keptRunning:
		glyph = "●"
		label = "kept"
		style = m.styles.chipUp
	case state.shutdownDone:
		glyph = "◯"
		label = "stopped"
		style = m.styles.chipDown
	case state.inactive:
		label = "inactive"
	case state.startPending:
		glyph = spinnerGlyph(m.spinnerFrame)
		label = "waiting"
		style = m.styles.chipWarn
	case state.starting:
		glyph = spinnerGlyph(m.spinnerFrame)
		label = "starting"
		style = m.styles.chipWarn
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
	if m.shuttingDown {
		var message string
		switch {
		case state.inactive:
			message = state.config.Name + " remains inactive."
		case state.stopping:
			message = "Stopping " + state.config.Name + " gracefully…"
		case state.shutdownErr != nil:
			message = "Failed to stop " + state.config.Name + ": " + state.shutdownErr.Error()
		case state.keptRunning:
			message = state.config.Name + " is managed by Compose and will remain running."
		case state.shutdownDone:
			message = state.config.Name + " stopped cleanly."
		default:
			message = "Waiting to stop " + state.config.Name + "…"
		}
		return lipgloss.NewStyle().Width(width).Render("  " + truncateText(message, max(20, width-4)))
	}

	if state.inactive {
		return "  Inactive · press s to start"
	}
	if state.startPending {
		return "  " + truncateText("Waiting for dependencies: "+strings.Join(state.config.DependsOn, ", "), max(0, width-4))
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
	if state.inactive {
		logContent = "Optional service. Press s to start it and its dependencies."
	} else if strings.TrimSpace(logContent) == "" {
		logContent = "No output yet. Waiting for the process to write to its log file."
	}
	innerW := max(8, width-4)
	innerH := max(2, height-2)
	logContent = clampLogTail(logContent, innerW, innerH)
	return m.styles.logBox.Width(width - 2).Height(height - 1).Render(logContent)
}

func (m model) actionHint() string {
	if state := m.selectedState(); state != nil && !state.running {
		if state.exitErr != nil {
			return "s retry start · r restart"
		}
		return "s start · r restart"
	}
	return "r restart"
}

func (m model) renderFooter(width int) string {
	keysText := "j/k move · m select text · " + m.actionHint() + " · q quit"
	if m.shuttingDown {
		keysText = "shutting down · q again hides progress"
	}
	if m.selectingText {
		keysText = "select text to copy · m resume · q quit"
	}
	keys := m.styles.muted.Render(keysText)
	rightText := m.logDir
	if m.shuttingDown {
		rightText = "please wait"
	}
	rightText = truncateText(rightText, max(0, width-lipgloss.Width(keys)-1))
	right := m.styles.muted.Render(rightText)
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
		return
	}
	state.lastLogTail = readLogTail(state.config.LogFile, 18)
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

func resetRestartBackoffAfterHealthy(state *serviceState, now time.Time) {
	if state == nil || !state.running || state.starting || state.restarting || state.exitErr != nil || state.liveFailures > 0 || state.restartAttempt == 0 || state.startedAt.IsZero() {
		return
	}
	if now.Sub(state.startedAt) >= 30*time.Second {
		state.restartAttempt = 0
		state.nextRestartAt = time.Time{}
	}
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
	if maxLines <= 0 {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		return "failed to read log: " + err.Error()
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "failed to read log: " + err.Error()
	}

	const chunkSize int64 = 32 * 1024
	position := info.Size()
	chunks := make([][]byte, 0, 4)
	newlines := 0
	for position > 0 && newlines <= maxLines {
		size := min(chunkSize, position)
		position -= size
		chunk := make([]byte, size)
		if _, err := file.ReadAt(chunk, position); err != nil {
			return "failed to read log: " + err.Error()
		}
		newlines += bytes.Count(chunk, []byte{'\n'})
		chunks = append(chunks, chunk)
	}

	var raw []byte
	for i := len(chunks) - 1; i >= 0; i-- {
		raw = append(raw, chunks[i]...)
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

type cappedLogWriter struct {
	mu       sync.Mutex
	file     *os.File
	maxBytes int64
	size     int64
}

func openCappedLogWriter(path string, maxBytes int64) (*cappedLogWriter, error) {
	if err := capLogFile(path, maxBytes); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &cappedLogWriter{file: file, maxBytes: maxBytes, size: info.Size()}, nil
}

func (w *cappedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	originalLength := len(p)
	if w.maxBytes > 0 && int64(len(p)) >= w.maxBytes {
		p = p[len(p)-int(w.maxBytes):]
		if err := w.file.Truncate(0); err != nil {
			return 0, err
		}
		w.size = 0
	} else if w.maxBytes > 0 && w.size+int64(len(p)) > w.maxBytes {
		retain := w.maxBytes*3/4 - int64(len(p))
		if retain < 0 {
			retain = 0
		}
		if retain > w.size {
			retain = w.size
		}
		tail := make([]byte, retain)
		if retain > 0 {
			if _, err := w.file.ReadAt(tail, w.size-retain); err != nil {
				return 0, err
			}
		}
		if err := w.file.Truncate(0); err != nil {
			return 0, err
		}
		if _, err := w.file.WriteAt(tail, 0); err != nil {
			return 0, err
		}
		w.size = retain
	}
	if _, err := w.file.Seek(w.size, 0); err != nil {
		return 0, err
	}
	written, err := w.file.Write(p)
	w.size += int64(written)
	if err != nil {
		return written, err
	}
	if written != len(p) {
		return written, io.ErrShortWrite
	}
	return originalLength, nil
}

func (w *cappedLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
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

	// Trim to 75% so a chatty process does not force a full rewrite on every
	// heartbeat as soon as it appends one byte beyond the configured cap.
	retainedBytes := maxBytes * 3 / 4
	start := info.Size() - retainedBytes
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

	return os.WriteFile(path, buf.Bytes(), 0o600)
}
