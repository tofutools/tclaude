// Package skillroots resolves the user-scope directories where tclaude
// installs skills for the supported agent harnesses.
package skillroots

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// All returns the lexical, absolute skill roots used by Claude Code and Codex
// CLI. Literal roots are preserved even when one is a symlink to another:
// harness permission matchers authorize the path the agent actually uses.
func All() ([]string, error) {
	claude, err := Claude()
	if err != nil {
		return nil, err
	}
	codex, err := Codex()
	if err != nil {
		return nil, err
	}
	return dedupe(append([]string{claude}, codex...)), nil
}

// Claude returns Claude Code's user-scope skill root.
func Claude() (string, error) {
	home, err := homeDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills"), nil
}

// Codex returns Codex CLI's supported user-scope skill roots.
func Codex() ([]string, error) {
	home, err := homeDirectory()
	if err != nil {
		return nil, err
	}

	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	codexHome, err = absolute(codexHome)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute CODEX_HOME for skill roots: %w", err)
	}

	return dedupe([]string{
		filepath.Join(home, ".agents", "skills"),
		filepath.Join(codexHome, "skills"),
	}), nil
}

func homeDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for skill roots: %w", err)
	}
	home, err = absolute(home)
	if err != nil {
		return "", fmt.Errorf("resolve absolute home directory for skill roots: %w", err)
	}
	return home, nil
}

func absolute(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(path), nil
}

func dedupe(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}
