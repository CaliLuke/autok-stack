package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfigDefaultsComposeCommand(t *testing.T) {
	dir := t.TempDir()
	writeStackConfig(t, dir, `
[stack]
required_tools = ["go", "podman"]

[[service]]
key = "db"
compose_file = "docker-compose.yml"
compose_services = ["postgres"]
`)
	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"podman", "compose"}
	if !reflect.DeepEqual(cfg.composeCommand, want) {
		t.Fatalf("composeCommand = %#v, want %#v", cfg.composeCommand, want)
	}
}

func TestLoadConfigRejectsUnsafeCleanupPatterns(t *testing.T) {
	dir := t.TempDir()
	writeStackConfig(t, dir, `
[stack]
cleanup_patterns = ["scripts/dev.sh"]

[[service]]
key = "app"
command = ["echo", "ok"]
`)
	_, err := loadConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("cleanup_patterns error = %v", err)
	}
}

func TestLoadConfigAcceptsComposeCommandOverride(t *testing.T) {
	dir := t.TempDir()
	writeStackConfig(t, dir, `
[stack]
required_tools = ["go", "docker"]
compose_command = ["docker", "compose"]

[[service]]
key = "db"
compose_file = "docker-compose.yml"
compose_services = ["postgres"]
`)
	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"docker", "compose"}
	if !reflect.DeepEqual(cfg.composeCommand, want) {
		t.Fatalf("composeCommand = %#v, want %#v", cfg.composeCommand, want)
	}
}

func TestFindConfigFileUsesDefaultDirWhenStartHasNoConfig(t *testing.T) {
	dir := t.TempDir()
	fallback := filepath.Join(dir, "fallback")
	other := filepath.Join(dir, "other")
	if err := os.MkdirAll(fallback, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStackConfig(t, fallback, `
[stack]

[[service]]
key = "app"
command = ["echo", "ok"]
`)
	t.Setenv(defaultDirEnv, fallback)

	got, err := findConfigFile(other)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(fallback, configFileName)
	if got != want {
		t.Fatalf("findConfigFile() = %q, want %q", got, want)
	}
}

func TestFindConfigFilePrefersNearestConfigOverDefaultDir(t *testing.T) {
	dir := t.TempDir()
	fallback := filepath.Join(dir, "fallback")
	project := filepath.Join(dir, "project")
	nested := filepath.Join(project, "nested")
	for _, path := range []string{fallback, nested} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeStackConfig(t, fallback, `
[stack]

[[service]]
key = "fallback"
command = ["echo", "fallback"]
`)
	writeStackConfig(t, project, `
[stack]

[[service]]
key = "project"
command = ["echo", "project"]
`)
	t.Setenv(defaultDirEnv, fallback)

	got, err := findConfigFile(nested)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(project, configFileName)
	if got != want {
		t.Fatalf("findConfigFile() = %q, want %q", got, want)
	}
}

func TestLoadConfigRejectsUnknownDependency(t *testing.T) {
	dir := t.TempDir()
	writeStackConfig(t, dir, `
[[service]]
key = "app"
command = ["echo", "ok"]
depends_on = ["missing"]
`)

	if _, err := loadConfig(dir); err == nil {
		t.Fatal("expected unknown dependency rejection")
	}
}

func TestLoadConfigRejectsDependencyCycle(t *testing.T) {
	dir := t.TempDir()
	writeStackConfig(t, dir, `
[[service]]
key = "api"
command = ["echo", "api"]
depends_on = ["db"]

[[service]]
key = "db"
command = ["echo", "db"]
depends_on = ["api"]
`)

	if _, err := loadConfig(dir); err == nil {
		t.Fatal("expected dependency cycle rejection")
	}
}

func TestBuildServiceConfigExpandsHomeCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := buildServiceConfig(t.TempDir(), t.TempDir(), serviceBlock{
		Key:     "tool",
		Command: []string{"~/bin/tool", "--flag"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "bin", "tool")
	if cfg.Command[0] != want {
		t.Fatalf("command path = %q, want %q", cfg.Command[0], want)
	}
}

func TestBuildServiceConfigKeepsBareDotCommandOnPath(t *testing.T) {
	cfg, err := buildServiceConfig(t.TempDir(), t.TempDir(), serviceBlock{
		Key:     "tool",
		Command: []string{"tool.dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Command[0] != "tool.dev" {
		t.Fatalf("bare command was rewritten: %q", cfg.Command[0])
	}
}

func TestBuildServiceConfigRejectsNonPositiveReadinessTimeout(t *testing.T) {
	for _, timeout := range []string{"0s", "-1s"} {
		t.Run(strings.ReplaceAll(timeout, "-", "negative-"), func(t *testing.T) {
			_, err := buildServiceConfig(t.TempDir(), t.TempDir(), serviceBlock{
				Key:              "app",
				Command:          []string{"app"},
				ReadinessTimeout: timeout,
			})
			if err == nil {
				t.Fatalf("expected %s to be rejected", timeout)
			}
		})
	}
}

func writeStackConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigPortEnvironmentReferences(t *testing.T) {
	root := t.TempDir()
	text := `[[service]]
key = "server"
ports = ["8000"]
command = ["server", "--port", "{{port}}"]
ready_url = "http://localhost:{{port}}/ready"
env = { API_PORT = "{{port}}" }
[[service]]
key = "client"
command = ["client"]
env = { API_URL = "http://localhost:{{port:server:8000}}" }
`
	if err := os.WriteFile(filepath.Join(root, "stack.toml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.services[0].Env["API_PORT"] != "{{port}}" || cfg.services[1].Env["API_URL"] != "http://localhost:{{port:server:8000}}" {
		t.Fatal("port templates were not preserved")
	}
}
