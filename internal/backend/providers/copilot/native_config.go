package copilot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/host"
)

// This provider-local native config editor retains the measured config.json
// trust semantics. It never resolves an ambient home or grants tool approval.
const CopilotConfigFileName = "config.json"
const copilotTrustedFoldersKey = "trustedFolders"

func ensureCopilotDirTrustedInHome(stateDir, projectDir string) error {
	if !filepath.IsAbs(projectDir) {
		return fmt.Errorf("copilot dir-trust: project dir %q is not absolute", projectDir)
	}
	configPath := filepath.Join(stateDir, CopilotConfigFileName)
	dirs := copilotTrustSpellings(projectDir)

	// Read-modify-write under the shared native config lock, not a bare atomic
	// write. The array is SHARED state: two concurrent spawns seeding different
	// directories both rewrite `trustedFolders`, and a last-writer-wins rename
	// would drop one of them — leaving that pane parked on the modal it was
	// seeded to clear. The plan runs inside the lock and is re-run from fresh
	// bytes if anything changed under it, so the two seeds compose.
	//
	// The default perm is 0600 — COPILOT_HOME sits beside a session store and
	// the CLI's own state, so a file tclaude creates there is private; an
	// existing file's mode is preserved by the editor.
	return host.EditNativeConfigFile("Copilot config", configPath, 0o600, func(configData []byte) (bool, []byte, error) {
		return planCopilotDirTrust(configData, dirs)
	})
}

func copilotTrustSpellings(projectDir string) []string {
	dirs := []string{filepath.Clean(projectDir)}
	if resolved, err := filepath.EvalSymlinks(dirs[0]); err == nil {
		if resolved = filepath.Clean(resolved); resolved != dirs[0] {
			dirs = append(dirs, resolved)
		}
	}
	return dirs
}

func planCopilotDirTrust(configData []byte, dirs []string) (bool, []byte, error) {
	config, err := parseCopilotSettingsObject(configData, CopilotConfigFileName)
	if err != nil {
		return false, nil, err
	}
	trusted, err := parseCopilotTrustedFolders(config[copilotTrustedFoldersKey])
	if err != nil {
		return false, nil, err
	}

	// Containment is compared on the CLEANED spelling of each existing entry
	// while the entry itself is carried over verbatim: a `/work/proj/` already
	// in the list is the same directory and must not be seeded twice, but
	// rewriting the operator's entries to tclaude's preferred spelling is not
	// this editor's business.
	existing := make([]string, 0, len(trusted))
	for _, folder := range trusted {
		existing = append(existing, filepath.Clean(strings.TrimSpace(folder)))
	}
	changed := false
	for _, dir := range dirs {
		if slices.Contains(existing, dir) {
			continue
		}
		trusted = append(trusted, dir)
		existing = append(existing, dir)
		changed = true
	}
	if !changed {
		return false, nil, nil
	}

	encoded, err := json.Marshal(trusted)
	if err != nil {
		return false, nil, fmt.Errorf("copilot dir-trust: encode %s: %w", copilotTrustedFoldersKey, err)
	}
	config[copilotTrustedFoldersKey] = json.RawMessage(encoded)

	// Every other key is carried across as its ORIGINAL bytes (RawMessage), so
	// no value in a file tclaude did not author is reformatted, re-escaped or
	// rounded. Key order is not preserved — Go marshals maps sorted — which is
	// immaterial for a JSON object the CLI rewrites on its next migration.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(config); err != nil {
		return false, nil, fmt.Errorf("copilot dir-trust: encode %s: %w", CopilotConfigFileName, err)
	}
	return true, buf.Bytes(), nil
}

func parseCopilotSettingsObject(data []byte, name string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	stripped := bytes.TrimSpace(stripCopilotLineComments(data))
	if len(stripped) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(stripped, &out); err != nil {
		return nil, fmt.Errorf("copilot dir-trust: cannot parse Copilot %s as a JSON object: %w; "+
			"fix the file, or clear the folder-trust prompt in the pane once", name, err)
	}
	if out == nil {
		return nil, fmt.Errorf("copilot dir-trust: cannot parse Copilot %s as a JSON object: top-level null is not an object; "+
			"fix the file, or clear the folder-trust prompt in the pane once", name)
	}
	return out, nil
}

func parseCopilotTrustedFolders(raw json.RawMessage) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var folders []string
	if err := json.Unmarshal(raw, &folders); err != nil {
		return nil, fmt.Errorf("copilot dir-trust: `%s` in Copilot %s is not an array of strings; "+
			"refusing to edit it", copilotTrustedFoldersKey, CopilotConfigFileName)
	}
	return folders, nil
}

func stripCopilotLineComments(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return []byte(strings.Join(kept, "\n"))
}
