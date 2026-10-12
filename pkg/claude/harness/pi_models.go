package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// piModels is pi's ModelCatalog.
//
// pi talks to dozens of providers and resolves `--model` itself: an exact
// `provider/id`, a bare id, or a fuzzy name, with an optional `:<thinking>`
// suffix. tclaude therefore validates only that the value is one bounded
// token of the characters model ids use, and leaves "which model" to pi,
// which reports an unknown one itself. No vendor is foreign to pi.
//
// Effort is pi's thinking level, rendered as `--thinking`; pi clamps it to
// what the selected model supports.
type piModels struct{}

const piMaxModelLen = 200

var piEffortLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

func (piModels) ValidateModel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > piMaxModelLen {
		return "", fmt.Errorf("invalid model: must be at most %d characters, got %d", piMaxModelLen, len(s))
	}
	if strings.HasPrefix(s, "-") {
		return "", fmt.Errorf("invalid model %q: must not start with '-'", s)
	}
	for _, r := range s {
		if !piModelRune(r) {
			return "", fmt.Errorf("invalid model %q: use pi's provider/id form (letters, digits and ._:/@+-)", s)
		}
	}
	return s, nil
}

func piModelRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("._:/@+-", r)
}

func (piModels) ValidateEffort(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || slices.Contains(piEffortLevels, s) {
		return s, nil
	}
	return "", fmt.Errorf("invalid effort %q for pi (want %s)", s, strings.Join(piEffortLevels, "|"))
}

func (piModels) EffortLevels() []string { return slices.Clone(piEffortLevels) }

// Models lists the models pi can use right now as provider/id: those whose
// provider has credentials, which is what `pi --list-models` prints. The
// result is cached briefly, because the spawn dialog asks often and pi is a
// Node process. It is nil when pi is not installed.
func (piModels) Models() []string {
	piModelList.Lock()
	defer piModelList.Unlock()
	if piModelList.at.IsZero() || time.Since(piModelList.at) > piModelListTTL {
		piModelList.models = listPiModels()
		piModelList.at = time.Now()
	}
	return slices.Clone(piModelList.models)
}

const piModelListTTL = 5 * time.Minute

var piModelList struct {
	sync.Mutex
	at     time.Time
	models []string
}

// listPiModels runs `pi --list-models` offline. Its stdin is /dev/null: pi
// reads a non-terminal stdin to its end before doing anything, so an inherited
// pipe would hang it.
var listPiModels = func() []string {
	path, err := exec.LookPath("pi")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--list-models")
	cmd.Env = append(os.Environ(), "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parsePiModelList(string(out))
}

// parsePiModelList reads `pi --list-models`' table: a header row starting with
// "provider model", then one row per model whose first two columns are the
// provider and the model id. Nothing before the header is a model: with no
// credentials pi prints a prose notice instead of a table.
func parsePiModelList(out string) []string {
	var models []string
	header := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "provider" && fields[1] == "model" {
			header = true
			continue
		}
		if !header || len(fields) < 2 {
			continue
		}
		model := fields[0] + "/" + fields[1]
		if _, err := (piModels{}).ValidateModel(model); err == nil && !slices.Contains(models, model) {
			models = append(models, model)
		}
	}
	return models
}
