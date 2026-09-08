//go:build linux || darwin

package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxSetupPreservesOrderedEnvironmentAndNativeArguments(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	child := ProcessSpec{Executable: "/bin/bash", Args: []string{"-c", `printf '%s\n' "$SETUP_VALUE" "$EMPTY_VALUE" "$1" "$2"`, "native", "$(touch " + marker + ")", SandboxControlFDArgument}, Env: []string{"PATH=/usr/bin:/bin"}}
	prepared, err := sandboxSetupCommand(child, []model.SandboxSetupBlock{
		{Name: "first", Script: `helper() { printf '%s' first; }; export SETUP_VALUE=$(helper); export EMPTY_VALUE=`, Exports: []string{"EMPTY_VALUE"}},
		{Name: "second", Script: `export SETUP_VALUE="$SETUP_VALUE:$(helper):second"; set -- replacement`, Exports: []string{"SETUP_VALUE"}},
	})
	require.NoError(t, err)
	require.NoFileExists(t, marker)
	cmd := exec.Command(prepared.Executable, prepared.Args...)
	cmd.Env = prepared.Env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	require.Equal(t, "first:first:second\n\n$(touch "+marker+")\n"+SandboxControlFDArgument+"\n", string(out))
	require.NoFileExists(t, marker)
}

func TestSandboxSetupRefusesFailureAndMissingExports(t *testing.T) {
	for _, setup := range []string{`false | true`, `helper() { false; }; helper`, `true`} {
		t.Run(setup, func(t *testing.T) {
			prepared, err := sandboxSetupCommand(ProcessSpec{Executable: "/bin/echo", Args: []string{"native-started"}}, []model.SandboxSetupBlock{{Name: "install", Script: setup, Exports: []string{"UNSET_SETUP_VALUE"}}})
			require.NoError(t, err)
			cmd := exec.Command(prepared.Executable, prepared.Args...)
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit)
			require.Equal(t, 126, exit.ExitCode())
			require.Contains(t, string(out), "pre-launch block 'install'")
			require.NotContains(t, string(out), "native-started")
		})
	}
}

func TestSandboxSetupIgnoresAmbientBashStartup(t *testing.T) {
	source := filepath.Join(t.TempDir(), "startup")
	require.NoError(t, os.WriteFile(source, []byte("echo unexpected-startup\n"), 0600))
	prepared, err := sandboxSetupCommand(ProcessSpec{Executable: "/bin/echo", Args: []string{"ready"}}, []model.SandboxSetupBlock{{Name: "empty", Script: "true"}})
	require.NoError(t, err)
	cmd := exec.Command(prepared.Executable, prepared.Args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "BASH_ENV=" + source}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	require.Equal(t, "ready", strings.TrimSpace(string(out)))
}
