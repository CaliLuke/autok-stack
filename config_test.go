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

func writeStackConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
