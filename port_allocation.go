package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// A plan belongs to one stack session. Allocate all endpoints before launching
// any process, including consumers that start before their providers.
type portPlan struct {
	mu          sync.Mutex
	initialized bool
	originals   map[string]serviceConfig
	order       []string
	assigned    map[string][]string
	declared    map[string]bool
}

func newPortPlan(configs []serviceConfig) *portPlan {
	p := &portPlan{originals: map[string]serviceConfig{}, assigned: map[string][]string{}, declared: map[string]bool{}}
	for _, cfg := range configs {
		cfg.portPlan = nil
		p.originals[cfg.Key] = cfg
		p.order = append(p.order, cfg.Key)
		p.assigned[cfg.Key] = slices.Clone(cfg.Ports)
		for _, port := range cfg.Ports {
			p.declared[port] = true
		}
	}
	return p
}

func (p *portPlan) prepare(ctx context.Context, key string) (serviceConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized {
		for _, service := range p.order {
			// An allocation failure affects that service, not independent branches.
			// The failing service retries below when its own launch begins.
			_ = p.allocate(ctx, service)
		}
		p.initialized = true
	}
	if err := p.allocate(ctx, key); err != nil {
		return serviceConfig{}, err
	}
	return p.resolve(key)
}

func (p *portPlan) allocate(ctx context.Context, key string) error {
	cfg := p.originals[key]
	if cfg.isComposeService() {
		return nil
	}
	cfg.Ports = slices.Clone(p.assigned[key])
	if err := ctx.Err(); err != nil {
		return err
	}
	occupied := map[string]bool{}
	for _, port := range cfg.Ports {
		if err := probePort(port); errors.Is(err, syscall.EADDRINUSE) {
			occupied[port] = true
		} else if err != nil {
			return fmt.Errorf("inspect port %s: %w", port, err)
		}
	}
	if len(occupied) > 0 && cfg.registry != nil {
		if err := ensurePortsAvailable(ctx, cfg); err != nil {
			var conflict *portContentionError
			if !errors.As(err, &conflict) {
				return err
			}
			for _, owner := range conflict.owners {
				occupied[owner.port] = true
			}
		} else {
			occupied = map[string]bool{}
		}
	}
	for i, port := range cfg.Ports {
		err := probePort(port)
		if err == nil && !occupied[port] && !p.usedByOther(key, port) {
			continue
		}
		if err != nil && !errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("inspect port %s: %w", port, err)
		}
		original := p.originals[key].Ports[i]
		if len(cfg.Ports) > 1 && !hasPortBinding(p.originals[key], original) {
			return fmt.Errorf("%s: port %s is occupied; bind it in command or env with {{port:%s}}", key, original, original)
		}
		number, _ := strconv.Atoi(port)
		found := false
		for n := 0; n < 65535; n++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			number++
			if number > 65535 {
				number = 1024
			}
			candidate := strconv.Itoa(number)
			if p.declared[candidate] || p.used(candidate) {
				continue
			}
			if err := probePort(candidate); err != nil {
				if errors.Is(err, syscall.EADDRINUSE) {
					continue
				}
				return fmt.Errorf("inspect port %s: %w", candidate, err)
			}
			p.assigned[key][i] = candidate
			found = true
			break
		}
		if !found {
			return fmt.Errorf("%s: no free TCP port for %s", key, original)
		}
	}
	return nil
}

func (p *portPlan) used(port string) bool {
	for _, ports := range p.assigned {
		if slices.Contains(ports, port) {
			return true
		}
	}
	return false
}

func (p *portPlan) usedByOther(key, port string) bool {
	for other, ports := range p.assigned {
		if other != key && slices.Contains(ports, port) {
			return true
		}
	}
	return false
}

// Bind wildcard addresses to catch listeners on any interface and either IP
// family. Probes close before exec: arbitrary programs cannot inherit listeners.
func probePort(port string) error {
	// On macOS a wildcard bind can coexist with a loopback-specific listener.
	// Probe both forms separately so SO_REUSEADDR cannot hide that conflict.
	for _, address := range []struct{ network, host string }{{"tcp4", ""}, {"tcp4", "127.0.0.1"}, {"tcp6", ""}, {"tcp6", "::1"}} {
		listener, err := net.Listen(address.network, net.JoinHostPort(address.host, port))
		if address.network == "tcp6" && (errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPROTONOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL)) {
			continue
		}
		if err != nil {
			return err
		}
		_ = listener.Close()
	}
	return nil
}

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var portReference = regexp.MustCompile(`\{\{port(?::[^{}]*)?\}\}`)

func (p *portPlan) expand(key, value string) (string, error) {
	var expandErr error
	result := portReference.ReplaceAllStringFunc(value, func(token string) string {
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(token, "{{"), "}}"), ":")
		service, port := key, ""
		switch len(parts) {
		case 1:
			if ports := p.originals[key].Ports; len(ports) > 0 {
				port = ports[0]
			}
		case 2:
			port = parts[1]
		case 3:
			service, port = parts[1], parts[2]
		default:
			expandErr = fmt.Errorf("invalid port reference %s", token)
			return token
		}
		original, ok := p.originals[service]
		if ok {
			if i := slices.Index(original.Ports, port); i >= 0 {
				return p.assigned[service][i]
			}
		}
		expandErr = fmt.Errorf("unknown port reference %s in service %s", token, key)
		return token
	})
	if strings.Contains(result, "{{port") && expandErr == nil {
		expandErr = fmt.Errorf("invalid port reference in %q", value)
	}
	return result, expandErr
}

func hasPortBinding(cfg serviceConfig, port string) bool {
	tokens := []string{"{{port:" + port + "}}", "{{port:" + cfg.Key + ":" + port + "}}"}
	if len(cfg.Ports) > 0 && cfg.Ports[0] == port {
		tokens = append(tokens, "{{port}}")
	}
	var values []string
	if len(cfg.Command) > 1 {
		values = slices.Clone(cfg.Command[1:])
	}
	for _, value := range cfg.Env {
		values = append(values, value)
	}
	for _, value := range values {
		for _, token := range tokens {
			if strings.Contains(value, token) {
				return true
			}
		}
	}
	return false
}

func (p *portPlan) resolve(key string) (serviceConfig, error) {
	cfg := p.originals[key]
	cfg.portPlan = p
	cfg.Ports = slices.Clone(p.assigned[key])
	cfg.Command = slices.Clone(cfg.Command)
	cfg.Env = maps.Clone(cfg.Env)
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	// PORT is the conventional default for a single-port command service.
	if !cfg.isComposeService() && len(cfg.Ports) == 1 {
		if _, explicit := cfg.Env["PORT"]; !explicit {
			cfg.Env["PORT"] = cfg.Ports[0]
		}
	}
	for i := 1; i < len(cfg.Command); i++ {
		value, err := p.expand(key, cfg.Command[i])
		if err != nil {
			return cfg, err
		}
		cfg.Command[i] = value
	}
	for name, value := range cfg.Env {
		expanded, err := p.expand(key, value)
		if err != nil {
			return cfg, err
		}
		cfg.Env[name] = expanded
	}
	for _, field := range []*string{&cfg.ReadyURL, &cfg.LiveURL} {
		explicit := portReference.MatchString(*field)
		value, err := p.expand(key, *field)
		if err != nil {
			return cfg, err
		}
		// Existing health URLs follow local port reassignment without config edits.
		parsed, err := url.Parse(value)
		if err == nil && !explicit && (parsed.Hostname() == "localhost" || net.ParseIP(parsed.Hostname()).IsLoopback()) {
			if i := slices.Index(p.originals[key].Ports, parsed.Port()); i >= 0 {
				parsed.Host = net.JoinHostPort(parsed.Hostname(), cfg.Ports[i])
				value = parsed.String()
			}
		}
		*field = value
	}
	return cfg, nil
}

func (p *portPlan) changed(cfg serviceConfig) bool {
	if !p.mu.TryLock() {
		return false
	}
	defer p.mu.Unlock()
	next, err := p.resolve(cfg.Key)
	return err == nil && (!maps.Equal(cfg.Env, next.Env) || !slices.Equal(cfg.Command, next.Command) || cfg.ReadyURL != next.ReadyURL || cfg.LiveURL != next.LiveURL)
}

func serviceEnvironment(cfg serviceConfig) []string {
	env := os.Environ()
	names := make([]string, 0, len(cfg.Env))
	for name := range cfg.Env {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		env = append(env, name+"="+cfg.Env[name])
	}
	return env
}

func validatePortConfig(configs []serviceConfig) error {
	p := newPortPlan(configs)
	for _, cfg := range configs {
		if cfg.isComposeService() && (len(cfg.Env) > 0 || strings.Contains(cfg.ReadyURL+cfg.LiveURL, "{{port")) {
			return fmt.Errorf("%s: env and port references are supported only for command services", cfg.Key)
		}
		seen := map[string]bool{}
		for _, port := range cfg.Ports {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
				return fmt.Errorf("%s: invalid TCP port %q", cfg.Key, port)
			}
			if seen[port] {
				return fmt.Errorf("%s: duplicate port %s", cfg.Key, port)
			}
			seen[port] = true
		}
		for name, value := range cfg.Env {
			if !environmentName.MatchString(name) || strings.ContainsRune(value, 0) {
				return fmt.Errorf("%s: invalid environment entry %q", cfg.Key, name)
			}
		}
		if len(cfg.Command) > 0 && strings.Contains(cfg.Command[0], "{{port") {
			return fmt.Errorf("%s: port references are not supported in command[0]", cfg.Key)
		}
		resolved, err := p.resolve(cfg.Key)
		if err != nil {
			return err
		}
		if err := validateHealthURL("ready_url", resolved.ReadyURL); err != nil {
			return fmt.Errorf("%s: %w", cfg.Key, err)
		}
		if err := validateHealthURL("live_url", resolved.LiveURL); err != nil {
			return fmt.Errorf("%s: %w", cfg.Key, err)
		}
	}
	return nil
}
