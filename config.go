package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

const configFileName = "stack.toml"
const defaultDirEnv = "STACK_DEFAULT_DIR"

// fileConfig mirrors the TOML layout. Paths are relative to the directory
// containing stack.toml unless they're already absolute.
type fileConfig struct {
	Stack    stackBlock     `toml:"stack"`
	Services []serviceBlock `toml:"service"`
}

type stackBlock struct {
	Title           string   `toml:"title"`
	LogDir          string   `toml:"log_dir"`
	RequiredTools   []string `toml:"required_tools"`
	ComposeCommand  []string `toml:"compose_command"`
	CleanupPatterns []string `toml:"cleanup_patterns"`
}

type serviceBlock struct {
	Key              string   `toml:"key"`
	Name             string   `toml:"name"`
	Ports            []string `toml:"ports"`
	WorkDir          string   `toml:"work_dir"`
	LogFile          string   `toml:"log_file"`
	Command          []string `toml:"command"`
	ComposeFile      string   `toml:"compose_file"`
	ComposeServices  []string `toml:"compose_services"`
	AutoRestart      bool     `toml:"auto_restart"`
	ReadinessTimeout string   `toml:"readiness_timeout"`
	ReadyURL         string   `toml:"ready_url"`
	LiveURL          string   `toml:"live_url"`
}

type loadedConfig struct {
	rootDir         string
	title           string
	logDir          string
	requiredTools   []string
	composeCommand  []string
	cleanupPatterns []string
	services        []serviceConfig
}

// findConfigFile walks up from start until it finds a stack.toml or hits the
// filesystem root. Returns the absolute path of the file.
func findConfigFile(start string) (string, error) {
	if path, err := findConfigFileUpward(start); err == nil {
		return path, nil
	}
	if fallback := os.Getenv(defaultDirEnv); fallback != "" {
		if path, err := findConfigFileUpward(fallback); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("no %s found in %s or any parent directory, and %s=%s did not contain one", configFileName, start, defaultDirEnv, fallback)
	}
	return "", fmt.Errorf("no %s found in %s or any parent directory", configFileName, start)
}

func findConfigFileUpward(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, configFileName)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found in %s or any parent directory", configFileName, start)
		}
		dir = parent
	}
}

func loadConfig(start string) (*loadedConfig, error) {
	path, err := findConfigFile(start)
	if err != nil {
		return nil, err
	}
	rootDir := filepath.Dir(path)

	var raw fileConfig
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if len(raw.Services) == 0 {
		return nil, fmt.Errorf("%s declares no [[service]] blocks", path)
	}

	title := raw.Stack.Title
	if title == "" {
		title = "STACK"
	}

	logDir := raw.Stack.LogDir
	if logDir == "" {
		logDir = filepath.Join(".tmp", "dev-stack")
	}
	logDir = resolvePath(rootDir, logDir)

	cleanupPatterns := make([]string, 0, len(raw.Stack.CleanupPatterns))
	for _, p := range raw.Stack.CleanupPatterns {
		cleanupPatterns = append(cleanupPatterns, resolvePath(rootDir, p))
	}

	composeCommand := raw.Stack.ComposeCommand
	if len(composeCommand) == 0 {
		composeCommand = []string{"podman", "compose"}
	}

	services := make([]serviceConfig, 0, len(raw.Services))
	seenKeys := make(map[string]struct{}, len(raw.Services))
	for i, s := range raw.Services {
		cfg, err := buildServiceConfig(rootDir, logDir, s)
		if err != nil {
			return nil, fmt.Errorf("service[%d]: %w", i, err)
		}
		if _, dup := seenKeys[cfg.Key]; dup {
			return nil, fmt.Errorf("service[%d]: duplicate key %q", i, cfg.Key)
		}
		seenKeys[cfg.Key] = struct{}{}
		services = append(services, cfg)
	}

	return &loadedConfig{
		rootDir:         rootDir,
		title:           title,
		logDir:          logDir,
		requiredTools:   raw.Stack.RequiredTools,
		composeCommand:  composeCommand,
		cleanupPatterns: cleanupPatterns,
		services:        services,
	}, nil
}

func buildServiceConfig(rootDir, logDir string, s serviceBlock) (serviceConfig, error) {
	if s.Key == "" {
		return serviceConfig{}, errors.New("missing key")
	}
	if s.Name == "" {
		s.Name = s.Key
	}

	isCompose := len(s.ComposeServices) > 0
	if isCompose && s.ComposeFile == "" {
		return serviceConfig{}, fmt.Errorf("%s: compose_services requires compose_file", s.Key)
	}
	if !isCompose && len(s.Command) == 0 {
		return serviceConfig{}, fmt.Errorf("%s: command is required (or set compose_services)", s.Key)
	}

	workDir := rootDir
	if s.WorkDir != "" {
		workDir = resolvePath(rootDir, s.WorkDir)
	}

	logFile := s.LogFile
	if logFile == "" {
		logFile = s.Key + ".log"
	}
	if !filepath.IsAbs(logFile) {
		logFile = filepath.Join(logDir, logFile)
	}

	command := make([]string, len(s.Command))
	for i, part := range s.Command {
		// Only resolve the first token if it looks like a relative path (starts with ./ or ../).
		// Resolve against the service's work_dir, not the stack root — the user's
		// `command = ["./dev.sh"]` is implicitly relative to where the service runs.
		// Bare commands like "bun" stay bare for PATH lookup.
		if i == 0 && looksLikeRelativePath(part) {
			command[i] = resolvePath(workDir, part)
		} else {
			command[i] = part
		}
	}

	composeFile := s.ComposeFile
	if composeFile != "" {
		composeFile = resolvePath(rootDir, composeFile)
	}

	var readiness time.Duration
	if s.ReadinessTimeout != "" {
		d, err := time.ParseDuration(s.ReadinessTimeout)
		if err != nil {
			return serviceConfig{}, fmt.Errorf("%s: invalid readiness_timeout: %w", s.Key, err)
		}
		readiness = d
	}
	if err := validateHealthURL("ready_url", s.ReadyURL); err != nil {
		return serviceConfig{}, fmt.Errorf("%s: %w", s.Key, err)
	}
	if err := validateHealthURL("live_url", s.LiveURL); err != nil {
		return serviceConfig{}, fmt.Errorf("%s: %w", s.Key, err)
	}

	return serviceConfig{
		Key:              s.Key,
		Name:             s.Name,
		Ports:            s.Ports,
		WorkDir:          workDir,
		LogFile:          logFile,
		Command:          command,
		ComposeFile:      composeFile,
		ComposeServices:  s.ComposeServices,
		AutoRestart:      s.AutoRestart,
		ReadinessTimeout: readiness,
		ReadyURL:         s.ReadyURL,
		LiveURL:          s.LiveURL,
	}, nil
}

func validateHealthURL(field, rawURL string) error {
	if rawURL == "" {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute HTTP(S) URL", field)
	}
	return nil
}

func resolvePath(rootDir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(rootDir, p)
}

func looksLikeRelativePath(s string) bool {
	return len(s) >= 2 && (s[0] == '.' || s[0] == '/' || s[0] == '~')
}
