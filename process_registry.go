package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const serviceWrapperArg = "__autok_stack_service_wrapper"

type processRecord struct {
	Key       string    `json:"key"`
	PID       int       `json:"pid"`
	PGID      int       `json:"pgid"`
	Token     string    `json:"token"`
	Command   []string  `json:"command"`
	WorkDir   string    `json:"work_dir"`
	LogFile   string    `json:"log_file"`
	StartedAt time.Time `json:"started_at"`
}

type processRegistry struct {
	mu      sync.Mutex
	path    string
	records map[string]processRecord
}

func openProcessRegistry(path string) (*processRegistry, error) {
	registry := &processRegistry{path: path, records: make(map[string]processRecord)}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return registry, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return registry, nil
	}
	if err := json.Unmarshal(raw, &registry.records); err != nil {
		return nil, fmt.Errorf("parse %s: %w (remove the file only after confirming no stack-owned processes remain)", path, err)
	}
	for key, record := range registry.records {
		if key == "" || record.Key != key || record.PID <= 0 || record.PGID <= 0 || record.Token == "" {
			return nil, fmt.Errorf("invalid process ownership record for %q in %s", key, path)
		}
	}
	return registry, nil
}

func newProcessOwnerToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (r *processRegistry) register(record processRecord) error {
	if r == nil {
		return nil
	}
	if record.Key == "" || record.PID <= 0 || record.PGID <= 0 || record.Token == "" {
		return errors.New("incomplete process ownership record")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	previous, hadPrevious := r.records[record.Key]
	r.records[record.Key] = record
	if err := r.writeLocked(); err != nil {
		if hadPrevious {
			r.records[record.Key] = previous
		} else {
			delete(r.records, record.Key)
		}
		return err
	}
	return nil
}

func (r *processRegistry) removeIfMatch(key string, pid int, token string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[key]
	if !ok || record.PID != pid || record.Token != token {
		return nil
	}
	delete(r.records, key)
	if err := r.writeLocked(); err != nil {
		r.records[key] = record
		return err
	}
	return nil
}

func (r *processRegistry) snapshot() []processRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	records := make([]processRecord, 0, len(r.records))
	for _, record := range r.records {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })
	return records
}

func (r *processRegistry) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(r.path), ".processes-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r.records); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, r.path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(r.path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// reapStale terminates only groups that still carry the exact random token
// recorded by a prior stack instance. A reused PGID is never sufficient proof.
func (r *processRegistry) reapStale(ctx context.Context) ([]string, error) {
	var warnings []string
	for _, record := range r.snapshot() {
		alive, owned, err := inspectProcessGroup(record)
		if err != nil {
			return warnings, fmt.Errorf("inspect stale %s process group %d: %w", record.Key, record.PGID, err)
		}
		if !alive {
			if err := r.removeIfMatch(record.Key, record.PID, record.Token); err != nil {
				return warnings, err
			}
			continue
		}
		if !owned {
			warnings = append(warnings, fmt.Sprintf("ignored reused process group %d from stale %s record because its ownership token did not match", record.PGID, record.Key))
			if err := r.removeIfMatch(record.Key, record.PID, record.Token); err != nil {
				return warnings, err
			}
			continue
		}
		if err := stopProcessGroupContext(ctx, record.PGID); err != nil {
			return warnings, fmt.Errorf("stop stale %s process group %d: %w", record.Key, record.PGID, err)
		}
		_ = appendSupervisorEvent(record.LogFile, "recovered stale process group pgid=%d started_at=%s after an unclean supervisor exit", record.PGID, record.StartedAt.Format(time.RFC3339Nano))
		if err := r.removeIfMatch(record.Key, record.PID, record.Token); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

func (r *processRegistry) reclaimOwnedPID(ctx context.Context, pid int) (bool, error) {
	pgid, err := syscall.Getpgid(pid)
	if errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, record := range r.snapshot() {
		if record.PGID != pgid {
			continue
		}
		alive, owned, err := inspectProcessGroup(record)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, r.removeIfMatch(record.Key, record.PID, record.Token)
		}
		if !owned {
			return false, nil
		}
		if err := stopProcessGroupContext(ctx, record.PGID); err != nil {
			return false, err
		}
		if err := r.removeIfMatch(record.Key, record.PID, record.Token); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func inspectProcessGroup(record processRecord) (alive bool, owned bool, err error) {
	members, err := processGroupMembers(record.PGID)
	if err != nil {
		return false, false, err
	}
	if len(members) == 0 {
		return false, false, nil
	}
	needle := serviceWrapperArg + " " + record.Token
	for _, pid := range members {
		command, err := exec.Command("ps", "ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				continue
			}
			return true, false, err
		}
		if strings.Contains(string(command), needle) {
			return true, true, nil
		}
	}
	return true, false, nil
}

func processGroupMembers(pgid int) ([]int, error) {
	output, err := exec.Command("ps", "-axo", "pid=,pgid=").Output()
	if err != nil {
		return nil, err
	}
	var members []int
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		group, groupErr := strconv.Atoi(fields[1])
		if pidErr == nil && groupErr == nil && group == pgid {
			members = append(members, pid)
		}
	}
	return members, nil
}

type portOwner struct {
	port    string
	pid     int
	command string
}

type portContentionError struct {
	details string
	owners  []portOwner
}

func (e *portContentionError) Error() string {
	return "refusing to terminate unrelated port owner; " + e.details
}

func ensurePortsAvailable(ctx context.Context, cfg serviceConfig) error {
	if len(cfg.Ports) == 0 {
		return nil
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		return fmt.Errorf("lsof is required to inspect service ports: %w", err)
	}

	owners, err := listeningPortOwners(ctx, cfg.Ports)
	if err != nil {
		return err
	}
	if len(owners) == 0 {
		return nil
	}

	reclaimed := false
	seenPIDs := make(map[int]struct{})
	for _, owner := range owners {
		if _, seen := seenPIDs[owner.pid]; seen || cfg.registry == nil {
			continue
		}
		seenPIDs[owner.pid] = struct{}{}
		owned, err := cfg.registry.reclaimOwnedPID(ctx, owner.pid)
		if err != nil {
			return fmt.Errorf("reclaim stack-owned listener pid %d: %w", owner.pid, err)
		}
		reclaimed = reclaimed || owned
	}
	if reclaimed {
		owners, err = listeningPortOwners(ctx, cfg.Ports)
		if err != nil {
			return err
		}
		if len(owners) == 0 {
			return nil
		}
	}

	details := make([]string, 0, len(owners))
	for _, owner := range owners {
		details = append(details, fmt.Sprintf("port %s: pid %d (%s)", owner.port, owner.pid, owner.command))
	}
	return &portContentionError{details: strings.Join(details, "; "), owners: owners}
}

func listeningPortOwners(ctx context.Context, ports []string) ([]portOwner, error) {
	owners := make(map[string]portOwner)
	for _, port := range ports {
		output, err := exec.CommandContext(ctx, "lsof", "-nP", "-tiTCP:"+port, "-sTCP:LISTEN").Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				continue
			}
			return nil, fmt.Errorf("inspect port %s: %w", port, err)
		}
		for _, line := range strings.Fields(string(output)) {
			pid, err := strconv.Atoi(line)
			if err != nil || pid <= 0 {
				continue
			}
			key := port + ":" + strconv.Itoa(pid)
			owners[key] = portOwner{port: port, pid: pid, command: processCommand(pid)}
		}
	}
	result := make([]portOwner, 0, len(owners))
	for _, owner := range owners {
		result = append(result, owner)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].port == result[j].port {
			return result[i].pid < result[j].pid
		}
		return result[i].port < result[j].port
	})
	return result, nil
}

func processCommand(pid int) string {
	output, err := exec.Command("ps", "ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil || strings.TrimSpace(string(output)) == "" {
		return "unknown command"
	}
	return strings.TrimSpace(string(output))
}

func appendSupervisorEvent(path, format string, args ...any) error {
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "[stack] at=%s "+format+"\n", append([]any{time.Now().Format(time.RFC3339Nano)}, args...)...)
	return err
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
