package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// This is a cleanup journal, not an agent identity. A dead launcher does not
// prove that descendants have stopped using its cache. Reuse the one-shot
// cgroup reap path; without that proof keep the tree until a different boot.
type oneShotDirectoryRun struct {
	Key     string `json:"key"`
	Base    string `json:"base"`
	Boot    string `json:"boot"`
	Started bool   `json:"started"`
	Cgroup  string `json:"cgroup,omitempty"`
}

var activeOneShotDirectories sync.Map
var oneShotBootIdentity = readOneShotBootIdentity

func readOneShotBootIdentity() string {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	} else if runtime.GOOS == "darwin" {
		data, err := exec.Command("sysctl", "-n", "kern.boottime").Output()
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return "" // Missing evidence must never authorize deletion.
}

func oneShotDirectoryJournalRoot() string {
	return filepath.Join(config.DataDir(), "one-shot-directories")
}

func newOneShotDirectoryRun() (*oneShotDirectoryRun, error) {
	base, err := canonicalizeForSecureMkdir(tclcommon.CacheDir())
	if err != nil {
		return nil, err
	}
	run := &oneShotDirectoryRun{Key: "oneshot-" + session.GenerateSessionID(), Base: filepath.Join(base, "agent-dirs"), Boot: oneShotBootIdentity()}
	activeOneShotDirectories.Store(run.Key, true)
	if err := run.save(); err != nil {
		activeOneShotDirectories.Delete(run.Key)
		return nil, err
	}
	return run, nil
}

func (run *oneShotDirectoryRun) save() error {
	root := oneShotDirectoryJournalRoot()
	if err := mkdirAllNoFollow(root, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	// The journal is private daemon state, outside every sandbox grant. Atomic
	// replacement means a crash leaves either the pre-launch or launched record.
	file, err := os.CreateTemp(root, ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(root, run.Key+".json"))
}

func (run *oneShotDirectoryRun) launched(cgroup string) error {
	run.Started, run.Cgroup = true, cgroup
	return run.save()
}

func (run *oneShotDirectoryRun) finish() {
	defer activeOneShotDirectories.Delete(run.Key)
	removed, err := run.reap()
	if err != nil {
		slog.Warn("one-shot directory cleanup failed", "key", run.Key, "error", err)
	}
	if !removed {
		slog.Warn("one-shot directories retained until termination can be proven", "path", filepath.Join(run.Base, run.Key), "journal", filepath.Join(oneShotDirectoryJournalRoot(), run.Key+".json"))
	}
}

func (run *oneShotDirectoryRun) reap() (bool, error) {
	if !strings.HasPrefix(run.Key, "oneshot-") || !agentDirectoryLaunchKeyRE.MatchString(run.Key) {
		return false, fmt.Errorf("invalid one-shot directory key")
	}
	base, err := canonicalizeForSecureMkdir(tclcommon.CacheDir())
	if err != nil {
		return false, err
	}
	if run.Base != filepath.Join(base, "agent-dirs") {
		return false, fmt.Errorf("one-shot directory root changed; retaining tree")
	}
	boot := oneShotBootIdentity()
	stopped := !run.Started || (boot != "" && run.Boot != "" && boot != run.Boot)
	if !stopped && run.Cgroup != "" {
		// A removed cgroup cannot contain a live process. Merely unreadable is
		// not proof; do not use the resource helper's best-effort populated probe.
		_, err := os.Stat(run.Cgroup)
		stopped = errors.Is(err, os.ErrNotExist)
	}
	if !stopped {
		return false, nil
	}
	if _, err := removeAgentOwnedDirAtNoFollow(run.Base, run.Key); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.Remove(filepath.Join(oneShotDirectoryJournalRoot(), run.Key+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

func cleanupRetainedOneShotDirectories() error {
	root := oneShotDirectoryJournalRoot()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "oneshot-") || !strings.HasSuffix(entry.Name(), ".json") || !entry.Type().IsRegular() {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".json")
		if _, active := activeOneShotDirectories.Load(key); active {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		var run oneShotDirectoryRun
		if len(data) > 4096 || json.Unmarshal(data, &run) != nil || run.Key != key {
			continue
		}
		// Reclaim an abandoned delegated boundary through the same validation and
		// reaping path as the helper. No group/process PID guess authorizes cleanup.
		if run.Started && run.Cgroup != "" && runtime.GOOS == "linux" {
			if session.ValidatePreparedResourceCgroup(run.Cgroup, sandboxpolicy.ResourceLimits{}) == nil {
				_ = session.RemoveResourceCgroup(run.Cgroup)
			}
		}
		if _, err := run.reap(); err != nil {
			slog.Warn("retained one-shot directory cleanup failed", "key", key, "error", err)
		}
	}
	return nil
}

func startOneShotDirectoryCleanup(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			if err := cleanupRetainedOneShotDirectories(); err != nil {
				slog.Warn("one-shot directory sweep failed", "error", err)
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
}
