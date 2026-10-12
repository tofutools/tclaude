package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// pi project trust, read from dist/core/trust-manager.js at PiMinimumVersion.
//
// A working directory that holds project-local pi resources (.pi/settings.json,
// .pi/mcp.json, .pi/extensions|skills|prompts|themes, .pi/SYSTEM.md,
// .pi/APPEND_SYSTEM.md, or .agents/skills in it or an ancestor) parks an
// interactive launch on a trust dialog until a decision exists. Decisions live
// in <agent dir>/trust.json, a flat object of canonical directory paths to
// true (trust), false (do not trust) or null (cleared):
//
//	{ "/abs/project": true, "/abs/other": false }
//
// The NEAREST entry walking up from the working directory decides. pi rejects
// the whole file on any other value, so this editor refuses a shape it does not
// understand rather than write one. pi serialises its own writes with a
// proper-lockfile directory at trust.json.lock; this editor takes that same
// lock, so a concurrent `/trust` in a pane cannot interleave with it. Like the
// other harnesses' editors it never overrides an explicit distrust.
const (
	piTrustFileName = "trust.json"
	piTrustStoreUI  = "~/.pi/agent/trust.json"
	// piTrustLockStale matches proper-lockfile's default: a lock directory
	// older than this belongs to a process that died holding it.
	piTrustLockStale   = 10 * time.Second
	piTrustLockTimeout = 5 * time.Second
)

// EnsurePiDirTrustedForLaunch records projectDir as trusted in the trust store
// the launch reads. getenv nil means the ambient environment.
func EnsurePiDirTrustedForLaunch(getenv func(string) string, projectDir string) error {
	if !filepath.IsAbs(projectDir) {
		return fmt.Errorf("pi dir-trust: project dir %q is not absolute", projectDir)
	}
	agentDir, err := piAgentDir(getenv)
	if err != nil {
		return fmt.Errorf("pi dir-trust: %w", err)
	}
	path := filepath.Join(agentDir, piTrustFileName)
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return fmt.Errorf("pi dir-trust: create %s: %w", agentDir, err)
	}
	release, err := acquirePiTrustLock(path + ".lock")
	if err != nil {
		return err
	}
	defer release()
	dirs := dirTrustSpellings(filepath.Clean(projectDir))
	return editHarnessConfigFile("pi trust store", path, 0o600,
		func(data []byte) (bool, []byte, error) {
			changed := false
			for _, dir := range dirs {
				added, next, err := planPiDirTrust(data, dir)
				if err != nil {
					return false, nil, err
				}
				if added {
					data, changed = next, true
				}
			}
			return changed, data, nil
		}, prepareAtomicWriteFile)
}

// planPiDirTrust trusts dir unless its nearest decision already trusts it.
func planPiDirTrust(data []byte, dir string) (bool, []byte, error) {
	entries := map[string]*bool{}
	if len(bytes.TrimSpace(data)) > 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
			return false, nil, fmt.Errorf("pi dir-trust: %s is not a JSON object; pi refuses it too, so fix it first", piTrustFileName)
		}
		for path, value := range raw {
			var decision *bool
			if err := json.Unmarshal(value, &decision); err != nil {
				return false, nil, fmt.Errorf("pi dir-trust: %s has a value for %q that is not true, false or null; "+
					"pi refuses the file, so fix it first", piTrustFileName, path)
			}
			entries[path] = decision
		}
	}
	for current := dir; ; {
		if decision, ok := entries[current]; ok && decision != nil {
			if *decision {
				return false, nil, nil
			}
			if current == dir {
				return false, nil, fmt.Errorf("pi dir-trust: %s marks %s as not trusted; "+
					"tclaude will not override an explicit distrust", piTrustFileName, dir)
			}
			break // a distrusted ancestor: an exact entry for dir outranks it
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	trusted := true
	entries[dir] = &trusted
	return true, marshalPiTrust(entries), nil
}

// marshalPiTrust writes keys sorted with two-space indentation and a trailing
// newline, the shape pi itself writes.
func marshalPiTrust(entries map[string]*bool) []byte {
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		key, _ := json.Marshal(k)
		value := "null"
		if entries[k] != nil {
			value = fmt.Sprint(*entries[k])
		}
		fmt.Fprintf(&b, "\n  %s: %s", key, value)
	}
	if len(keys) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// acquirePiTrustLock takes pi's own mkdir-based lock on the trust store.
func acquirePiTrustLock(lockDir string) (func(), error) {
	deadline := time.Now().Add(piTrustLockTimeout)
	for {
		err := os.Mkdir(lockDir, 0o700)
		if err == nil {
			return func() { _ = os.Remove(lockDir) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("pi dir-trust: lock %s: %w", lockDir, err)
		}
		if info, statErr := os.Stat(lockDir); statErr == nil && time.Since(info.ModTime()) > piTrustLockStale {
			_ = os.Remove(lockDir)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("pi dir-trust: lock %s: timed out after %s", lockDir, piTrustLockTimeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
