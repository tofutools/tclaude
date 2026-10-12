package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withPiExtensionCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := piExtensionCacheDir
	piExtensionCacheDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { piExtensionCacheDir = prev })
	return dir
}

func TestMaterializePiExtension(t *testing.T) {
	root := withPiExtensionCache(t)
	path, err := MaterializePiExtension()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, root+string(filepath.Separator)) || filepath.Base(path) != piExtensionFile {
		t.Fatalf("path %q outside %q", path, root)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, PiExtensionSource()) {
		t.Fatalf("materialized bytes differ (err %v)", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("extension is writable: %v", info.Mode())
	}
	again, err := MaterializePiExtension()
	if err != nil || again != path {
		t.Fatalf("second call = %q, %v", again, err)
	}
}

func TestMaterializePiExtensionReplacesTamperedCopy(t *testing.T) {
	withPiExtensionCache(t)
	path, err := MaterializePiExtension()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("export default () => {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializePiExtension(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, PiExtensionSource()) {
		t.Fatal("tampered extension was loaded instead of replaced")
	}
}

func TestMaterializePiExtensionReplacesSymlink(t *testing.T) {
	withPiExtensionCache(t)
	path, err := MaterializePiExtension()
	if err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(t.TempDir(), "decoy.ts")
	if err := os.WriteFile(decoy, PiExtensionSource(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(decoy, path); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializePiExtension(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("symlink left in place: %v %v", info, err)
	}
}

// The extension is pi's only status channel; the descriptor must expose it so
// session new materializes it for every launch.
func TestPiDeclaresStatusExtension(t *testing.T) {
	if MustGet(PiName).StatusExtension == nil {
		t.Fatal("pi has no status extension")
	}
	src := string(PiExtensionSource())
	for _, want := range []string{PiHookBinEnvVar, `"session", "hook-callback"`, "shell: false"} {
		if !strings.Contains(src, want) {
			t.Errorf("extension source lacks %s", want)
		}
	}
}

func TestPiSpawnerLoadsStatusExtension(t *testing.T) {
	got := piSpawner{}.BuildCommand(SpawnSpec{
		EnvExports:          "export TCLAUDE_SESSION_ID=s1; ",
		StatusExtensionPath: "/cache/tclaude/pi-extension/ab/tclaude-pi.ts",
		TclaudeExecutable:   "/opt/my tools/tclaude",
		SessionID:           "u1",
	})
	want := "export TCLAUDE_SESSION_ID=s1; export TCLAUDE_PI_HOOK_BIN='/opt/my tools/tclaude'; " +
		"pi --extension /cache/tclaude/pi-extension/ab/tclaude-pi.ts --session-id u1"
	if got != want {
		t.Fatalf("command:\n got %s\nwant %s", got, want)
	}
}

func TestMaterializePiExtensionReplacesSymlinkedHashDir(t *testing.T) {
	root := withPiExtensionCache(t)
	path, err := MaterializePiExtension()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, piExtensionFile), PiExtensionSource(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	got, err := MaterializePiExtension()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Dir(got)); err != nil || !info.IsDir() || filepath.Dir(got) != dir {
		t.Fatalf("hash directory under %s is still a symlink: %v %v", root, info, err)
	}
}
