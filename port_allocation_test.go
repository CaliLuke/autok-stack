package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Re-exec the Go test binary so integration tests need no external HTTP server.
func TestPortServerHelper(t *testing.T) {
	if os.Getenv("STACK_PORT_TEST_HELPER") != "1" {
		return
	}
	port := os.Getenv("PORT")
	if custom := os.Getenv("TEST_BIND_PORT"); custom != "" {
		port = custom
	}
	if err := http.ListenAndServe("127.0.0.1:"+port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "service on "+port)
	})); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func occupiedTestPort(t *testing.T) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
}

func portTestConfig(t *testing.T, key, port string) serviceConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return serviceConfig{Key: key, Ports: []string{port}, Command: []string{exe, "-test.run=^TestPortServerHelper$"}, Env: map[string]string{"STACK_PORT_TEST_HELPER": "1"}, LogFile: filepath.Join(t.TempDir(), key+".log"), ReadinessTimeout: 3 * time.Second, ReadyURL: "http://127.0.0.1:" + port + "/ready", LiveURL: "http://localhost:" + port + "/live"}
}

func TestOccupiedPortBootAndRestartUseAssignedEndpoint(t *testing.T) {
	listener, port := occupiedTestPort(t)
	cfg := portTestConfig(t, "server", port)
	cfg.Env["TEST_BIND_PORT"] = "{{port}}"
	exitCh := make(chan processExitMsg, 10)
	services, order, failed := bootServices(context.Background(), []serviceConfig{cfg}, exitCh, 1024*1024)
	t.Cleanup(func() { _ = shutdownServices(services, order) })
	state := services[cfg.Key]
	if failed {
		t.Fatalf("boot failed: %v", state.exitErr)
	}
	assigned := state.config.Ports[0]
	if assigned == port {
		t.Fatal("occupied port was retained")
	}
	if state.config.Env["TEST_BIND_PORT"] != assigned || !strings.Contains(state.config.ReadyURL, ":"+assigned+"/") {
		t.Fatalf("stale config: %+v", state.config)
	}
	if err := checkServiceHealthOnce(context.Background(), state.config); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal("unrelated listener was disrupted:", err)
	}
	_ = conn.Close()
	tracker := newOperationTracker()
	msg := restartServiceCmd(context.Background(), tracker, cfg.Key, state.config, state.process, 2, 1024*1024, exitCh)().(restartResultMsg)
	if msg.next != nil {
		services[cfg.Key] = msg.next
		tracker.release(msg.next.process)
	}
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if msg.next.config.Ports[0] != assigned {
		t.Fatal("ordinary restart changed assigned port")
	}
}

func TestPortPlanAllocatesDistinctPortsAndResolvesConsumers(t *testing.T) {
	_, port := occupiedTestPort(t)
	configs := []serviceConfig{
		{Key: "one", Ports: []string{port}, Command: []string{"server", "--port", "{{port}}"}},
		{Key: "two", Ports: []string{port}, Command: []string{"server"}},
		{Key: "client", Command: []string{"client"}, Env: map[string]string{"URL": "http://localhost:{{port:one:" + port + "}}/api"}},
	}
	p := newPortPlan(configs)
	var wg sync.WaitGroup
	results := make(chan serviceConfig, len(configs))
	for _, cfg := range configs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, err := p.prepare(context.Background(), cfg.Key)
			if err != nil {
				t.Error(err)
				return
			}
			results <- next
		}()
	}
	wg.Wait()
	close(results)
	resolved := map[string]serviceConfig{}
	for cfg := range results {
		resolved[cfg.Key] = cfg
	}
	one, two := resolved["one"].Ports[0], resolved["two"].Ports[0]
	if one == two || one == port || two == port {
		t.Fatalf("ports: %s %s occupied=%s", one, two, port)
	}
	if resolved["one"].Command[2] != one || resolved["client"].Env["URL"] != "http://localhost:"+one+"/api" {
		t.Fatalf("unresolved consumer: %+v", resolved)
	}
	if configs[0].Command[2] != "{{port}}" {
		t.Fatal("mutated source config")
	}
}

func TestPortPlanPreservesFreeAndComposePorts(t *testing.T) {
	listener, port := occupiedTestPort(t)
	_ = listener.Close()
	_, composePort := occupiedTestPort(t)
	configs := []serviceConfig{{Key: "app", Ports: []string{port}, Command: []string{"app"}}, {Key: "db", Ports: []string{composePort}, ComposeServices: []string{"db"}}}
	p := newPortPlan(configs)
	cfg, err := p.prepare(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ports[0] != port || p.assigned["db"][0] != composePort {
		t.Fatal("changed available or compose ports")
	}
}

func TestMultiPortAllocationOnlyChangesOccupiedListener(t *testing.T) {
	_, busy := occupiedTestPort(t)
	listener, free := occupiedTestPort(t)
	_ = listener.Close()
	cfg := serviceConfig{Key: "collector", Ports: []string{busy, free}, Command: []string{"collector"}, Env: map[string]string{"GRPC": "127.0.0.1:{{port:" + busy + "}}", "HTTP": "{{port:" + free + "}}"}}
	next, err := newPortPlan([]serviceConfig{cfg}).prepare(context.Background(), cfg.Key)
	if err != nil {
		t.Fatal(err)
	}
	if next.Ports[0] == busy || next.Ports[1] != free || next.Env["GRPC"] != "127.0.0.1:"+next.Ports[0] {
		t.Fatalf("bad assignment: %+v", next)
	}
	delete(cfg.Env, "GRPC")
	if _, err := newPortPlan([]serviceConfig{cfg}).prepare(context.Background(), cfg.Key); err == nil || !strings.Contains(err.Error(), "bind it") {
		t.Fatalf("unbound listener error: %v", err)
	}
}

func TestPortChangeRefreshesRunningConsumer(t *testing.T) {
	_, port := occupiedTestPort(t)
	configs := []serviceConfig{{Key: "app", Ports: []string{port}, Command: []string{"app"}}, {Key: "client", Command: []string{"client"}, DependsOn: []string{"app"}, Env: map[string]string{"URL": "http://localhost:{{port:app:" + port + "}}"}}}
	p := newPortPlan(configs)
	app, err := p.prepare(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	client, err := p.prepare(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	thief, err := net.Listen("tcp4", "127.0.0.1:"+app.Ports[0])
	if err != nil {
		t.Fatal(err)
	}
	defer thief.Close()
	replacement, err := p.prepare(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Ports[0] == app.Ports[0] || !p.changed(client) {
		t.Fatal("consumer did not notice reassignment")
	}
	m := model{services: map[string]*serviceState{"app": {config: replacement, running: true}, "client": {config: client, running: true}}, runtimeCtx: context.Background(), operations: newOperationTracker(), healthBusy: true}
	m.Update(tickMsg(time.Now()))
	if !m.services["client"].restarting {
		t.Fatal("running consumer was not scheduled for restart")
	}
}

func TestPortConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  serviceConfig
	}{
		{"zero", serviceConfig{Ports: []string{"0"}}},
		{"range", serviceConfig{Ports: []string{"65536"}}},
		{"text", serviceConfig{Ports: []string{"http"}}},
		{"duplicate", serviceConfig{Ports: []string{"8000", "8000"}}},
		{"unknown reference", serviceConfig{Env: map[string]string{"URL": "{{port:missing:8000}}"}}},
		{"broken reference", serviceConfig{Env: map[string]string{"URL": "{{port"}}},
		{"bad environment", serviceConfig{Env: map[string]string{"BAD=NAME": "value"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Key = "app"
			if err := validatePortConfig([]serviceConfig{tc.cfg}); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestPortAllocationHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newPortPlan([]serviceConfig{{Key: "app", Ports: []string{"8000"}, Command: []string{"app"}}}).prepare(ctx, "app")
	if err != context.Canceled {
		t.Fatalf("error=%v", err)
	}
}

func TestPortPlanReservesOtherServicesPreferredPorts(t *testing.T) {
	_, busy := occupiedTestPort(t)
	n, _ := strconv.Atoi(busy)
	if n == 65535 {
		t.Skip("no following preferred port")
	}
	next := strconv.Itoa(n + 1)
	cfgs := []serviceConfig{{Key: "app", Ports: []string{busy}, Command: []string{"app"}}, {Key: "other", Ports: []string{next}, Command: []string{"other"}}}
	cfg, err := newPortPlan(cfgs).prepare(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ports[0] == next {
		t.Fatal("fallback took another service's preferred port")
	}
}

func TestPortPlanDetectsIPv6Listener(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 unavailable: %v", err)
	}
	defer listener.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	cfg, err := newPortPlan([]serviceConfig{{Key: "app", Ports: []string{port}, Command: []string{"app"}}}).prepare(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ports[0] == port {
		t.Fatal("IPv6 conflict was missed")
	}
}

func TestRestartReassignsPortTakenWhileServiceWasStopped(t *testing.T) {
	_, port := occupiedTestPort(t)
	cfg := portTestConfig(t, "server", port)
	exits := make(chan processExitMsg, 8)
	state, err := bootService(context.Background(), cfg, exits, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stopProcessGroup(state.pid) }()
	previous := state.config.Ports[0]
	if err := stopProcessGroup(state.pid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exits:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not exit")
	}
	thief, err := net.Listen("tcp4", "127.0.0.1:"+previous)
	if err != nil {
		t.Fatal(err)
	}
	defer thief.Close()
	tracker := newOperationTracker()
	result := restartServiceCmd(context.Background(), tracker, cfg.Key, state.config, nil, 2, 1024*1024, exits)().(restartResultMsg)
	if result.next != nil {
		state = result.next
		tracker.release(state.process)
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	if state.config.Ports[0] == previous {
		t.Fatal("restart retained stolen port")
	}
	if err := checkServiceHealthOnce(context.Background(), state.config); err != nil {
		t.Fatal(err)
	}
}

func TestPortPlanLeavesUnrelatedRegistryListenerRunning(t *testing.T) {
	listener, port := occupiedTestPort(t)
	registry, err := openProcessRegistry(filepath.Join(t.TempDir(), "processes.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := serviceConfig{Key: "app", Ports: []string{port}, Command: []string{"app"}, registry: registry}
	resolved, err := newPortPlan([]serviceConfig{cfg}).prepare(context.Background(), cfg.Key)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Ports[0] == port {
		t.Fatal("occupied port was not remapped")
	}
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal("unrelated listener was disrupted:", err)
	}
	_ = conn.Close()
}
