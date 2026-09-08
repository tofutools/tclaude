//go:build linux || darwin

package host

import (
	"fmt"
	"os"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// sandboxSetupCommand runs authored setup inside the same OS boundary as the
// native command. Native arguments remain positional data, including inherited
// control descriptor placeholders; setup cannot replace them with `set --`.
func sandboxSetupCommand(child ProcessSpec, blocks []model.SandboxSetupBlock) (ProcessSpec, error) {
	if len(blocks) == 0 {
		return child, nil
	}
	const bash = "/bin/bash"
	info, err := os.Stat(bash)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return ProcessSpec{}, fmt.Errorf("sandbox setup requires /bin/bash")
	}
	var script strings.Builder
	script.WriteString(`readonly -a tclaude_setup_command=("$@")
tclaude_setup_failed() { printf "tclaude: pre-launch block '%s' failed; refusing harness launch\n" "$tclaude_setup_block" >&2; exit 126; }
tclaude_setup_require() { local n; for n in "$@"; do
 if [ -z "${!n+x}" ]; then
  printf "tclaude: pre-launch block '%s' declares export '%s' but did not set it\n" "$tclaude_setup_block" "$n" >&2
  exit 126
 fi
done; }
set -eEo pipefail
trap 'tclaude_setup_failed' ERR
`)
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	for _, block := range blocks {
		script.WriteString("tclaude_setup_block=" + quote(block.Name) + "\n")
		script.WriteString(block.Script)
		script.WriteString("\n")
		if len(block.Exports) != 0 {
			script.WriteString("tclaude_setup_require")
			for _, name := range block.Exports {
				script.WriteString(" " + quote(name))
			}
			script.WriteString("\n")
		}
	}
	script.WriteString("trap - ERR\nset +eEo pipefail\nunset tclaude_setup_block\nexec \"${tclaude_setup_command[@]}\"\n")
	// Privileged mode suppresses BASH_ENV and inherited shell functions. The
	// process retains its existing uid/gid; no privilege is acquired here.
	args := []string{"--noprofile", "--norc", "-p", "-c", script.String(), "tclaude-sandbox-setup", child.Executable}
	child.Args = append(args, child.Args...)
	child.Executable = bash
	return child, nil
}
