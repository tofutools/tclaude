package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gofrs/flock"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// This provider-local native config editor retains the measured config.json
// trust semantics. It never resolves an ambient home or grants tool approval.
const CopilotConfigFileName = "config.json"
const copilotTrustedFoldersKey = "trustedFolders"
const copilotConfigEditMaxAttempts = 5
const copilotConfigLockRetry = 50 * time.Millisecond

var copilotConfigEditMu sync.Mutex
var copilotConfigLockTimeout = 10 * time.Second

type atomicFileReplacement struct{ path, tmpName, dir string }

func ensureCopilotDirTrustedInHome(stateDir, projectDir string) error {
	if !filepath.IsAbs(projectDir) {
		return fmt.Errorf("copilot dir-trust: project dir %q is not absolute", projectDir)
	}
	configPath := filepath.Join(stateDir, CopilotConfigFileName)
	dirs := copilotTrustSpellings(projectDir)

	// Read-modify-write under EditCopilotConfigFile's lock, not a bare atomic
	// write. The array is SHARED state: two concurrent spawns seeding different
	// directories both rewrite `trustedFolders`, and a last-writer-wins rename
	// would drop one of them — leaving that pane parked on the modal it was
	// seeded to clear. The plan runs inside the lock and is re-run from fresh
	// bytes if anything changed under it, so the two seeds compose.
	//
	// The default perm is 0600 — COPILOT_HOME sits beside a session store and
	// the CLI's own state, so a file tclaude creates there is private; an
	// existing file's mode is preserved by the editor.
	return EditCopilotConfigFile(configPath, 0o600, func(configData []byte) (bool, []byte, error) {
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

func EditCopilotConfigFile(
	configPath string,
	defaultPerm os.FileMode,
	plan func([]byte) (bool, []byte, error),
) error {
	return editHarnessConfigFile("Copilot config", configPath, defaultPerm, plan, prepareAtomicWriteFile)
}

func editHarnessConfigFile(
	label string,
	configPath string,
	defaultPerm os.FileMode,
	plan func([]byte) (bool, []byte, error),
	prepare func(string, []byte, os.FileMode) (*atomicFileReplacement, error),
) error {
	copilotConfigEditMu.Lock()
	defer copilotConfigEditMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return fmt.Errorf("create %s directory: %w", label, err)
	}
	fileLock := flock.New(configPath + ".tclaude.lock")
	lockCtx, cancelLock := context.WithTimeout(context.Background(), copilotConfigLockTimeout)
	defer cancelLock()
	locked, err := fileLock.TryLockContext(lockCtx, copilotConfigLockRetry)
	if err != nil {
		return fmt.Errorf("lock %s: %w", label, err)
	}
	if !locked {
		return fmt.Errorf("lock %s: timed out after %s", label, copilotConfigLockTimeout)
	}
	defer func() { _ = fileLock.Unlock() }()

	for attempt := 1; attempt <= copilotConfigEditMaxAttempts; attempt++ {
		target, err := atomicWriteTarget(configPath)
		if err != nil {
			return fmt.Errorf("resolve %s target: %w", label, err)
		}
		before, err := readFileAllowMissing(target)
		if err != nil {
			return fmt.Errorf("read %s: %w", label, err)
		}
		changed, out, err := plan(before)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}

		perm := defaultPerm
		if fi, statErr := os.Stat(target); statErr == nil {
			perm = fi.Mode().Perm()
		}
		replacement, err := prepare(target, out, perm)
		if err != nil {
			return err
		}

		// A non-tclaude writer cannot honor our advisory lock. Recheck both
		// the symlink target and bytes after the replacement has been fully
		// staged, then re-plan from the new state if either changed.
		currentTarget, err := atomicWriteTarget(configPath)
		if err != nil {
			replacement.discard()
			return fmt.Errorf("recheck %s target: %w", label, err)
		}
		current, err := readFileAllowMissing(currentTarget)
		if err != nil {
			replacement.discard()
			return fmt.Errorf("recheck %s: %w", label, err)
		}
		if currentTarget != target || !bytes.Equal(current, before) {
			replacement.discard()
			continue
		}
		if err := replacement.commit(); err != nil {
			replacement.discard()
			return err
		}
		return nil
	}
	return fmt.Errorf("%s kept changing during edit; retry later", label)
}

func readFileAllowMissing(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

func atomicWriteTarget(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, nil
		}
		return "", err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve symlink %s: %w", path, err)
	}
	return target, nil
}

func prepareAtomicWriteFile(path string, data []byte, perm os.FileMode) (*atomicFileReplacement, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	replacement := &atomicFileReplacement{path: path, tmpName: tmpName, dir: dir}
	ok := false
	defer func() {
		if !ok {
			replacement.discard()
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return nil, fmt.Errorf("chmod temp config: %w", err)
	}
	ok = true
	return replacement, nil
}

func (r *atomicFileReplacement) discard() {
	if r != nil && r.tmpName != "" {
		_ = os.Remove(r.tmpName)
	}
}

func (r *atomicFileReplacement) commit() error {
	if err := os.Rename(r.tmpName, r.path); err != nil {
		return fmt.Errorf("rename temp config into place: %w", err)
	}
	r.tmpName = ""
	// fsync the parent directory so the rename itself is durable across a hard
	// crash (the file content is already fsync'd above). Best-effort: a
	// directory that can't be opened/synced doesn't undo the successful write.
	if d, derr := os.Open(r.dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
