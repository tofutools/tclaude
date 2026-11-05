package agentd

import (
	"net/http"
	"os"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// reserveSpawnLabel is executeSpawn's label step. It keeps a label the
// deferred server continuation already reserved, or mints one from p.Name. A
// launch behind tclaude's sandbox also reserves or prepares its private
// attachment root under that label (refusing a root another spawn generation
// owns) and promotes the daemon-staged attachments into it.
//
// attachmentCleanup is the private-attachment cleanup executeSpawn holds so
// far; the returned cleanup replaces it on every path, error paths included,
// so the caller's deferred cleanup removes exactly what was created.
func reserveSpawnLabel(p *spawnParams, layeredLaunch bool, attachmentCleanup func()) (string, func(), *spawnFailure) {
	privateAttachmentCleanup := attachmentCleanup
	label := p.pendingSpawnLabel
	if label == "" && layeredLaunch {
		nextLabel := spawnLabelSequence(p.Name)
		var reserveErr error
		label, privateAttachmentCleanup, reserveErr =
			reserveUniqueSpawnPrivateAttachmentRootWith(nextLabel)
		if reserveErr != nil {
			return label, privateAttachmentCleanup, &spawnFailure{
				http.StatusInternalServerError,
				"spawn",
				"could not reserve private attachment root: " + reserveErr.Error(),
			}
		}
	} else if label == "" {
		label = spawnLabelSequence(p.Name)()
	} else if layeredLaunch {
		_, privateRootCreated, prepareErr :=
			tclcommon.PrepareSpawnAttachmentsPrivateDir(label)
		if prepareErr != nil {
			return label, privateAttachmentCleanup, &spawnFailure{
				http.StatusInternalServerError,
				"spawn",
				"could not prepare private attachment root: " + prepareErr.Error(),
			}
		}
		if !privateRootCreated && !p.privateAttachmentRootReserved {
			return label, privateAttachmentCleanup, &spawnFailure{
				http.StatusConflict,
				"spawn_label_collision",
				"private attachment root is already owned by another spawn generation",
			}
		}
		privateAttachmentCleanup = func() {
			_ = os.Remove(tclcommon.SpawnAttachmentsPrivateDir(label))
		}
	}
	if layeredLaunch {
		if len(p.Attachments) > 0 {
			promoted, batchCleanup, promoteErr :=
				promoteSpawnAttachments(label, p.Attachments)
			if promoteErr != nil {
				return label, privateAttachmentCleanup, &spawnFailure{
					http.StatusBadRequest,
					"invalid_attachments",
					"could not promote daemon-staged attachments: " + promoteErr.Error(),
				}
			}
			rootCleanup := privateAttachmentCleanup
			privateAttachmentCleanup = func() {
				batchCleanup()
				rootCleanup()
			}
			p.Attachments = promoted
		}
	}
	return label, privateAttachmentCleanup, nil
}

// prepareSpawnStateRoot is executeSpawn's Codex-state step: it mints the
// agent id a Codex app-server launch needs up front, resolves the Codex state
// root and its source, and records the fast-mode value a Codex launch starts
// with under that root.
func prepareSpawnStateRoot(p *spawnParams) *spawnFailure {
	if p.CodexAppServer && p.AgentID == "" {
		p.AgentID = db.NewAgentID()
	}
	stateRoot, stateRootSource, stateRootErr := codexStateRootForLaunch(p.Harness, p.EffectiveSandbox)
	if stateRootErr != nil {
		return &spawnFailure{http.StatusUnprocessableEntity, "codex_state_root", stateRootErr.Error()}
	}
	p.CodexStateRoot, p.CodexStateRootSource = stateRoot, stateRootSource
	if harnessOrDefault(p.Harness) == harness.CodexName {
		p.FastModeAtLaunch = codexFastModeAtLaunch(
			fastModeLaunchValue(p.FastMode, p.FastModeSet), p.CodexStateRoot)
	}
	return nil
}
