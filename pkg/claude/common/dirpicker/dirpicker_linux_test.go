//go:build linux

package dirpicker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// These exercise runLinuxPicker's exit-convention mapping with stand-in
// binaries, so the cancel-vs-failure-vs-context distinction is covered
// without a real GUI picker.

func TestRunLinuxPicker_OutputOnCleanExit(t *testing.T) {
	out, err := runLinuxPicker(context.Background(), "/bin/echo", []string{"/some/dir"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "/some/dir" {
		t.Fatalf("got %q, want %q", out, "/some/dir")
	}
}

func TestRunLinuxPicker_Exit1IsCancel(t *testing.T) {
	// /bin/false exits 1 with no output — the cancel convention.
	_, err := runLinuxPicker(context.Background(), "/bin/false", nil, "")
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("got %v, want ErrCanceled", err)
	}
}

func TestRunLinuxPicker_RealFailureSurfaced(t *testing.T) {
	// Non-1 exit + stderr + no stdout = a genuine failure, not a cancel.
	_, err := runLinuxPicker(context.Background(), "/bin/sh",
		[]string{"-c", "echo boom >&2; exit 2"}, "")
	if err == nil || errors.Is(err, ErrCanceled) {
		t.Fatalf("got %v, want a real error", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error %q should carry stderr", err)
	}
}

func TestEnvWithOverride_DropsDuplicateKey(t *testing.T) {
	// A pre-existing value must not survive: getenv returns the first match,
	// so a leftover entry would shadow our override.
	t.Setenv("GDK_BACKEND", "wayland")
	env := envWithOverride("GDK_BACKEND=x11")

	var matches []string
	for _, e := range env {
		if strings.HasPrefix(e, "GDK_BACKEND=") {
			matches = append(matches, e)
		}
	}
	if len(matches) != 1 || matches[0] != "GDK_BACKEND=x11" {
		t.Fatalf("GDK_BACKEND entries = %v, want exactly [GDK_BACKEND=x11]", matches)
	}
}

func TestRunLinuxPicker_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runLinuxPicker(ctx, "/bin/sleep", []string{"5"}, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
