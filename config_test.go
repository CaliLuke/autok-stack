package main

import (
	"os"
	"path/filepath"
	"reflect"
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

func writeStackConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
