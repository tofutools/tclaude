package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var piVersionRE = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)

// CheckPiVersion refuses a pi older than PiMinimumVersion, the version every
// contract tclaude relies on was verified against: --session-id creating a
// session, the extension events behind live status, and the session format.
// stdin is /dev/null: pi reads a piped stdin to its end before doing anything.
func CheckPiVersion(binary string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = append(os.Environ(), "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("cannot determine the pi version (%s --version: %v); tclaude requires pi %s or newer",
			binary, err, PiMinimumVersion)
	}
	found := strings.TrimSpace(string(out))
	if !piVersionAtLeast(found, PiMinimumVersion) {
		return fmt.Errorf("pi %s is too old: tclaude requires pi %s or newer; upgrade with "+
			"`npm install --global @earendil-works/pi-coding-agent@latest`", firstLine(found), PiMinimumVersion)
	}
	return nil
}

// piVersionAtLeast compares the leading major.minor.patch of found with min.
// An unparsable version is not at least anything.
func piVersionAtLeast(found, min string) bool {
	f, m := piVersionRE.FindStringSubmatch(found), piVersionRE.FindStringSubmatch(min)
	if f == nil || m == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		a, _ := strconv.Atoi(f[i])
		b, _ := strconv.Atoi(m[i])
		if a != b {
			return a > b
		}
	}
	return true
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if line == "" {
		return "(unknown)"
	}
	return line
}
