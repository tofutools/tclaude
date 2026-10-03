package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTestRunner(t *testing.T) {
	runner := filepath.Join(moduleRoot(t), "scripts", "test.sh")
	for _, tc := range []struct {
		name         string
		args         []string
		wantArgs     []string
		exit         string
		wantExit     int
		relativeTemp bool
	}{
		{"default", nil, []string{"test", "./..."}, "0", 0, false},
		{"arguments", []string{"./some/package", "-run", "a test with spaces", "-count=1"}, []string{"test", "./some/package", "-run", "a test with spaces", "-count=1"}, "0", 0, false},
		{"failure", []string{"./some/package"}, []string{"test", "./some/package"}, "23", 23, false},
		{"relative-temp", nil, []string{"test", "./..."}, "0", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			report := filepath.Join(root, "report")
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			// Replace only the Go subprocess boundary. Inspect the real wrapper's
			// environment while its temporary directory is still alive.
			shim := `#!/usr/bin/env bash
set -eu
[ -L "$TMPDIR" ] || exit 90
[ -d "$TMPDIR" ] || exit 91
printf '%s\0' "$TMPDIR" "$@" > "$TEST_RUNNER_REPORT"
exit "$TEST_RUNNER_EXIT"
`
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(shim), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			tempDir := root
			if tc.relativeTemp {
				tempDir = "."
			}
			t.Setenv("TMPDIR", tempDir)
			t.Setenv("TEST_RUNNER_REPORT", report)
			t.Setenv("TEST_RUNNER_EXIT", tc.exit)
			cmd := exec.Command("bash", append([]string{runner}, tc.args...)...)
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			exit := 0
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatal(err)
				}
				exit = ee.ExitCode()
			}
			if exit != tc.wantExit {
				t.Fatalf("exit %d, want %d: %s", exit, tc.wantExit, out)
			}
			data, err := os.ReadFile(report)
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			if !reflect.DeepEqual(parts[1:], tc.wantArgs) {
				t.Fatalf("arguments %q, want %q", parts[1:], tc.wantArgs)
			}
			if _, err := os.Lstat(filepath.Dir(parts[0])); !os.IsNotExist(err) {
				t.Fatalf("scratch directory survived: %v", err)
			}
			if os.Getenv("TMPDIR") != tempDir {
				t.Fatal("wrapper changed caller TMPDIR")
			}
		})
	}
}
