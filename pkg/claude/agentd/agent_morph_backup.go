package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/renametitle"
)

// A pre-morph backup is a clone of the agent in its current form (harness,
// model, profile, history), taken just before a morph applies and left
// stopped, so the operator can wake it if the morphed agent disappoints.
//
// It is a separate, standalone agent: no group memberships, permission
// overrides or group ownerships. Two live identities in the same groups
// would both receive group mail and could both act on the same work; a
// backup is meant to sit still until someone deliberately wakes it.

// morphBackupSuffix names a backup after its source.
const morphBackupSuffix = " (pre-morph backup)"

// morphBackup is the backup a morph left behind.
type morphBackup struct {
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Name    string `json:"name"`
	// Warning is set when the backup exists but did not settle as asked
	// (it could not be renamed or stopped).
	Warning string `json:"warning,omitempty"`
}

// morphBackupTitle is "<source title> (pre-morph backup)", trimmed to the
// rename length cap so the title stays injectable.
func morphBackupTitle(sourceConv string) string {
	base := ""
	if t, ok := harnessNativeTitle(sourceConv); ok {
		base = t
	} else if row := agent.FreshConvRowResolved(sourceConv); row != nil {
		base = agent.DisplayTitle(row)
	}
	// Runes the rename gate refuses become '-', so the backup can always be
	// named; double spaces collapse for the same reason.
	base = strings.Map(func(r rune) rune {
		if renametitle.CharsetOK(string(r)) {
			return r
		}
		return '-'
	}, base)
	base = strings.TrimSpace(strings.Join(strings.Fields(base), " "))
	if base == "" {
		base = "agent"
	}
	if room := renametitle.MaxLen - len(morphBackupSuffix); len(base) > room {
		base = strings.TrimSpace(base[:max(room, 1)])
	}
	return base + morphBackupSuffix
}

// errMorphBackupOffline refuses a backup of an agent without a live pane:
// the clone machinery copies the conversation from the running agent.
var errMorphBackupOffline = errors.New("a pre-morph backup clones the running agent, and it is offline; wake it first or morph without a backup")

// spawnMorphBackup clones sourceConv in its current form and stops the
// clone. A failure before the clone exists is an error and the morph must
// not go ahead; once the clone exists, rename or stop trouble is a warning.
func spawnMorphBackup(sourceConv string) (*morphBackup, error) {
	live := pickAliveSession(sourceConv)
	if live == nil {
		return nil, errMorphBackupOffline
	}
	relaunch, err := durableRelaunchConfigForConv(sourceConv)
	if err != nil {
		return nil, fmt.Errorf("read the agent's launch form: %w", err)
	}
	cwd, err := livePaneCwd(live.TmuxSession)
	if err != nil {
		return nil, fmt.Errorf("resolve the agent's working directory: %w", err)
	}
	gitCommonDir, err := spawnGitCommonDir(relaunch.Harness, sandboxForHarness(relaunch.Harness), sandboxImplementationForConv(sourceConv), cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve the agent's git directory: %w", err)
	}
	title := morphBackupTitle(sourceConv)
	spawned, cerr := cloneSpawnOnce(cloneSpawnParams{
		SourceConv:        sourceConv,
		Cwd:               cwd,
		Effort:            relaunch.Effort,
		Model:             relaunch.Model,
		CodexGitCommonDir: gitCommonDir,
		Title:             title,
	})
	if cerr != nil {
		return nil, fmt.Errorf("clone the agent: %s", cerr.Msg)
	}
	defer spawned.ReleaseLaunchClaim()
	backup := &morphBackup{ConvID: spawned.NewConv, Name: title}
	var warnings []string
	if spawned.Warn != "" {
		warnings = append(warnings, spawned.Warn)
	}
	// An agent identity makes the backup visible and wakeable like any
	// agent. Nothing of the source's identity is copied onto it.
	if agentID, _, err := db.EnsureAgentForConv(spawned.NewConv, "morph-backup"); err != nil {
		warnings = append(warnings, "could not register the backup as an agent: "+err.Error())
	} else {
		backup.AgentID = agentID
	}
	if waitForConvPaneAlive(spawned.NewConv) {
		if !spawned.LaunchEnrolled && renametitle.ValidSink(title) {
			time.Sleep(reincarnateReadyDelay)
			if !deliverRename(spawned.NewConv, title) {
				warnings = append(warnings, "could not rename the backup")
			}
		}
		if _, outcome := stopOneConvAndWait(spawned.NewConv, false, db.AgentExitActionStop, "", 0); outcome == softExitStuck || outcome == softExitUnattempted {
			warnings = append(warnings, "the backup could not be stopped; it is running idle")
		}
	} else {
		warnings = append(warnings, "the backup's pane did not come online; wake it to use it")
	}
	if len(warnings) > 0 {
		backup.Warning = strings.Join(warnings, "; ")
		slog.Warn("morph: backup clone", "source", short8(sourceConv), "backup", short8(spawned.NewConv), "warning", backup.Warning)
	}
	return backup, nil
}
