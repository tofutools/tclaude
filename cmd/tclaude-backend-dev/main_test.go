package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegisteredProvidersComposeAllHarnessesWithoutLaunching(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"claude", "codex", "opencode", "copilot"} {
		// Constructors resolve native executables, but must not start a workload.
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 91\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := t.TempDir()
	registry, err := registeredProviders(state, []string{"claude", "codex", "opencode", "copilot"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "codex", "opencode", "copilot"} {
		p, ok := registry.Provider(name)
		if !ok || p.Name() != name {
			t.Fatalf("provider %s not composed", name)
		}
	}
	if _, err := registeredProviders(state, []string{"codex", "codex"}); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	if _, err := registeredProviders(state, []string{"unknown"}); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestDevelopmentInitializationDoesNotRequireNativeExecutables(t *testing.T) {
	state := filepath.Join(t.TempDir(), "new")
	cmd := command()
	cmd.SetArgs([]string{"--init", "--state-dir", state, "--harness", "codex,copilot"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex", "copilot"} {
		if _, err := os.Stat(filepath.Join(state, name)); !os.IsNotExist(err) {
			t.Fatalf("initialization created native state for %s: %v", name, err)
		}
	}
}

func TestJourneyHistorySourcesAreCompositionOwned(t *testing.T) {
	services, err := journeyServices(t.TempDir(), []string{"claude", "opencode", "codex"}, []string{"claude:archive=/tmp/disposable-history"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	scope, ok := services.History.HistorySource("claude", "archive")
	if !ok || scope.Source != "/tmp/disposable-history" {
		t.Fatal("named source not resolved")
	}
	if _, ok := services.History.HistorySource("opencode", "owned"); !ok {
		t.Fatal("owned source missing")
	}
	if _, ok := services.History.HistorySource("claude", "/tmp/disposable-history"); ok {
		t.Fatal("raw path accepted as source name")
	}
	for _, source := range []string{"claude:x=relative", "copilot:x=/tmp/history", "codex:x=/tmp/history", "opencode:owned=/tmp/history"} {
		if _, err := journeyServices(t.TempDir(), []string{"claude", "opencode", "codex"}, []string{source}, false, ""); err == nil {
			t.Fatalf("accepted invalid source %s", source)
		}
	}
}
