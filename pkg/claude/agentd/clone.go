package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// cloneSpawnError carries enough context to surface either an HTTP
// error (when called from the single-clone handler) or accumulate
// into a per-member result (when called from groups-clone). The two
// callers differ in how they report failure but agree on which
// statuses + codes apply.
type cloneSpawnError struct {
	Status int
	Code   string
	Msg    string
}

func (e *cloneSpawnError) Error() string { return e.Msg }
func (e *cloneSpawnError) write(w http.ResponseWriter) {
	writeError(w, e.Status, e.Code, e.Msg)
}

// cloneSpawnOnce mints a clone's conv-id (and optionally its jsonl).
// Two branches:
//   - copy: use convops to fork the existing jsonl onto a fresh
//     conv-id; spawn `tclaude session new -r <new-conv>` so CC loads
//     the cloned conversation.
//   - no-copy: spawn `tclaude session new --label <label>` and poll
//     for whatever conv-id CC mints, same as reincarnate.
//
// Returns (newConv, newTmux, label, warn, nil) on success. label may
// be empty in the copy path when the session row's id field hasn't
// materialised within the deadline; that's not an error since the
// conv-id is already known. warn is a non-empty string when the spawn
// succeeded but the tmux session never registered within the polling
// deadline (copy path only) — the caller surfaces it as a HTTP
// response `warning` field so the dashboard can show "started but not
// online yet" instead of a generic success toast.
//
// effort and model are the launch flags for the clone's CC instance —
// callers pass the source's inherited flags (inheritedLaunchFlags) so
// the clone runs the same model as the original; "" omits the flag.
//
// Extracted from runCloneOrchestration so groups-clone can reuse the
// same race handling without duplicating it.
// proofDirs, when non-empty, are the write-proof-verified cwd and/or repository
// roots. cloneSpawnOnce re-asserts they are still canonical immediately before
// each fork, closing the verify→launch window the same way executeSpawn does.
func cloneSpawnOnce(sourceConv, cwd string, noCopyConv bool, effort, model, proofToken string, proofCwd bool, proofDirs []string, codexGitCommonDir string, gitWriteDirs []string) (newConv, newTmux, label, warn string, spawnErr *cloneSpawnError) {
	effectiveSandbox, err := db.AgentEffectiveSandboxConfigForConv(sourceConv)
	if err != nil {
		return "", "", "", "", &cloneSpawnError{Status: http.StatusInternalServerError, Code: "io", Msg: "load source sandbox snapshot: " + err.Error()}
	}
	if effectiveSandbox != nil {
		validated, err := ensureAgentDirectoriesForRelaunch(*effectiveSandbox)
		if err != nil {
			return "", "", "", "", &cloneSpawnError{Status: http.StatusConflict, Code: "sandbox_profile_changed", Msg: err.Error()}
		}
		effectiveSandbox = &validated
		launchFilesystem, launchErr := sandboxpolicy.FilesystemForLaunch(validated.Effective)
		if launchErr != nil {
			return "", "", "", "", &cloneSpawnError{Status: http.StatusConflict, Code: "sandbox_profile_changed", Msg: launchErr.Error()}
		}
		if proofToken != "" {
			proved := make(map[string]bool, len(proofDirs))
			for _, dir := range proofDirs {
				proved[dir] = true
			}
			for _, grant := range launchFilesystem {
				if grant.Access == sandboxpolicy.AccessWrite && !proved[grant.Path] {
					return "", "", "", "", &cloneSpawnError{Status: http.StatusForbidden, Code: "write_proof_failed", Msg: "sandbox profile write root was not included in the clone write proof: " + grant.Path}
				}
			}
		}
	}
	reassertFail := func() *cloneSpawnError {
		if fail := reassertDirWriteProof(proofDirs); fail != nil {
			return &cloneSpawnError{Status: fail.Status, Code: fail.Kind, Msg: fail.Msg}
		}
		return nil
	}
	// Clone under the same harness the source ran on — a Codex agent's
	// clone must relaunch as Codex. "" for an untagged/claude source omits
	// the flag (the default).
	srcHarness := harnessForConv(sourceConv).Name
	// Carry the source's armed Remote Access to the sibling (JOH-261). A clone
	// becoming a SECOND phone-reachable session alongside the still-running
	// original is the operator-decided semantics — drive either from the phone.
	// False (and so omitted) for an unarmed source or a Codex source.
	remoteControl := remoteControlForRelaunch(sourceConv, srcHarness)
	cloneSandbox := sandboxForHarness(srcHarness)
	codexGitCommonDirPinned := spawnUsesPinnedGitCommonDir(srcHarness, cloneSandbox)
	if codexGitCommonDirPinned && gitWriteDirs == nil {
		if home, err := os.UserHomeDir(); err == nil {
			gitWriteDirs = harness.GitWorktreeWriteDirs(cwd, codexGitCommonDir, home)
		}
	}
	var grantFail *spawnFailure
	gitWriteDirs, grantFail = canonicalizeRepositoryWriteDirs(gitWriteDirs, proofDirs, proofToken)
	if grantFail != nil {
		return "", "", "", "", &cloneSpawnError{Status: grantFail.Status, Code: grantFail.Kind, Msg: grantFail.Msg}
	}
	exactGrantPinned := codexGitCommonDirPinned && proofToken != ""
	if !exactGrantPinned {
		gitWriteDirs = nil
		codexGitCommonDir = ""
		codexGitCommonDirPinned = false
	}
	// Preserve the source's per-agent AskUserQuestion timeout onto the sibling
	// (schema v97) — unlike sandbox/approval, which re-default. "" for a source
	// that recorded none (a non-Claude or pre-column source).
	askTimeout := askTimeoutForRelaunch(sourceConv)
	if noCopyConv {
		label = generateSpawnLabel()
		agentDirectoryCleanup := func() {}
		if effectiveSandbox != nil && len(effectiveSandbox.Effective.AgentDirectories) > 0 {
			materialized, cleanup, materializeErr := materializeAgentDirectories(*effectiveSandbox, label)
			if materializeErr != nil {
				return "", "", "", "", &cloneSpawnError{Status: http.StatusInternalServerError, Code: "spawn", Msg: materializeErr.Error()}
			}
			effectiveSandbox = &materialized
			agentDirectoryCleanup = cleanup
		}
		// A clone is a relaunch, not a fresh opt-in, so it never engages the
		// experimental auto-review guardian (autoReview=false) nor pre-trusts the
		// cwd (trustDir=false — that edits ~/.codex/config.toml and is an explicit
		// fresh-spawn opt-in) — same rationale as approvalForHarness re-defaulting
		// rather than carrying per-conv state.
		if fail := reassertFail(); fail != nil {
			agentDirectoryCleanup()
			return "", "", "", "", fail
		}
		proofArgs := clcommon.SpawnArgs{DirWriteProof: proofToken, EffectiveSandbox: effectiveSandbox}
		if proofCwd {
			proofArgs.CwdWriteProof = proofToken
			proofArgs.DirWriteProof = ""
		}
		proofArgs.Label = label
		proofArgs.Cwd = cwd
		proofArgs.Effort = effort
		proofArgs.Model = model
		proofArgs.Harness = srcHarness
		proofArgs.Sandbox = cloneSandbox
		proofArgs.CodexGitCommonDir = codexGitCommonDir
		proofArgs.CodexGitCommonDirPinned = codexGitCommonDirPinned
		proofArgs.GitWorktreeWriteDirs = gitWriteDirs
		proofArgs.GitWorktreeWriteDirsPinned = exactGrantPinned
		proofArgs.Approval = approvalForHarness(srcHarness)
		proofArgs.AskUserQuestionTimeout = askTimeout
		proofArgs.RemoteControl = remoteControl
		if err := SpawnDetachedTclaudeNew(proofArgs); err != nil {
			agentDirectoryCleanup()
			return "", "", "", "", &cloneSpawnError{
				Status: http.StatusInternalServerError, Code: "spawn",
				Msg: "failed to launch tclaude session new: " + err.Error(),
			}
		}
		deadline := time.Now().Add(reincarnateSpawnTimeout)
		for time.Now().Before(deadline) {
			s, err := db.LoadSession(label)
			if err == nil && s != nil {
				newTmux = s.TmuxSession
				if s.ConvID != "" {
					// Tag the sibling row's best-known remote-control ON (JOH-261);
					// the --remote-control launch flag already armed its pane.
					if remoteControl {
						armRemoteControlOnNewRow(label)
					}
					return s.ConvID, newTmux, label, "", nil
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		slog.Warn("clone: no-copy poll timed out; conv-id never appeared",
			"label", label, "deadline", reincarnateSpawnTimeout)
		return "", newTmux, label, "", &cloneSpawnError{
			Status: http.StatusGatewayTimeout, Code: "timeout",
			Msg: "spawned session " + label + " but conv-id never materialised within " +
				reincarnateSpawnTimeout.String() +
				" — the session may still come up; check `tclaude session attach " + label + "`",
		}
	}
	// Copy path: fork the jsonl first, then resume into it.
	copyResult, err := convops.CopyConversationToPath(sourceConv, cwd, true /* global */)
	if err != nil {
		return "", "", "", "", &cloneSpawnError{
			Status: http.StatusInternalServerError, Code: "copy",
			Msg: "failed to copy conversation jsonl: " + err.Error(),
		}
	}
	newConv = copyResult.NewConvID
	agentDirectoryCleanup := func() {}
	if effectiveSandbox != nil && len(effectiveSandbox.Effective.AgentDirectories) > 0 {
		materialized, cleanup, materializeErr := materializeAgentDirectories(*effectiveSandbox, newConv)
		if materializeErr != nil {
			return "", "", "", "", &cloneSpawnError{Status: http.StatusInternalServerError, Code: "spawn", Msg: materializeErr.Error()}
		}
		effectiveSandbox = &materialized
		agentDirectoryCleanup = cleanup
	}
	if fail := reassertFail(); fail != nil {
		agentDirectoryCleanup()
		return "", "", "", "", fail
	}
	proofArgs := clcommon.SpawnArgs{DirWriteProof: proofToken, EffectiveSandbox: effectiveSandbox}
	if proofCwd {
		proofArgs.CwdWriteProof = proofToken
		proofArgs.DirWriteProof = ""
	}
	proofArgs.ConvID = newConv
	proofArgs.Cwd = cwd
	proofArgs.Effort = effort
	proofArgs.Model = model
	proofArgs.Harness = srcHarness
	proofArgs.Sandbox = cloneSandbox
	proofArgs.CodexGitCommonDir = codexGitCommonDir
	proofArgs.CodexGitCommonDirPinned = codexGitCommonDirPinned
	proofArgs.GitWorktreeWriteDirs = gitWriteDirs
	proofArgs.GitWorktreeWriteDirsPinned = exactGrantPinned
	proofArgs.Approval = approvalForHarness(srcHarness)
	proofArgs.AskUserQuestionTimeout = askTimeout
	proofArgs.RemoteControl = remoteControl
	if err := SpawnDetachedTclaudeResume(proofArgs); err != nil {
		agentDirectoryCleanup()
		return "", "", "", "", &cloneSpawnError{
			Status: http.StatusInternalServerError, Code: "spawn",
			Msg: "failed to launch tclaude session new -r: " + err.Error(),
		}
	}
	deadline := time.Now().Add(reincarnateSpawnTimeout)
	for time.Now().Before(deadline) {
		if s, err := db.FindSessionByConvID(newConv); err == nil && s != nil && s.TmuxSession != "" {
			newTmux = s.TmuxSession
			if s.ID != "" {
				label = s.ID
			}
			// Tag the sibling row's best-known remote-control ON (JOH-261); the
			// --remote-control launch flag (on the resume) already armed its
			// pane. The copy path discovers the label from the row (s.ID), so
			// tag only when it materialised — an empty label can't be keyed.
			if remoteControl && label != "" {
				armRemoteControlOnNewRow(label)
			}
			return newConv, newTmux, label, "", nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	// Spawn was best-effort fire-and-forget: the conv-id is already
	// known and the .jsonl exists, so we don't fail the request. But
	// we DO surface a warning — silently returning success here is the
	// "clone modal sat for 30s then showed a toast but nothing appeared"
	// trap. Logs (~/.tclaude/output.log) and the captured subprocess
	// stderr (see liveSpawnResume) tell the rest of the story.
	warn = fmt.Sprintf("spawned tclaude session for %s but its tmux session never registered within %s — the new agent may still come online; check ~/.tclaude/output.log for subprocess errors",
		short8(newConv), reincarnateSpawnTimeout)
	slog.Warn("clone: copy-path poll timed out; tmux session never registered",
		"new_conv", newConv, "deadline", reincarnateSpawnTimeout)
	return newConv, newTmux, label, warn, nil
}

// defaultCloneCooldown is the built-in fallback for CloneCooldown when
// neither the `--agent-clone-cooldown` serve flag nor the
// agent.clone_cooldown config field is set. resolveCloneCooldown
// returns it as the lowest-priority tier.
const defaultCloneCooldown = time.Minute

// CloneCooldown is the minimum time between two clones of the same
// source conv. The clone handler does an atomic INSERT-WHERE-NOT-
// EXISTS against agent_clone_history to enforce this — see
// db.ClaimCloneSlot. Defaults to defaultCloneCooldown; `tclaude agentd
// serve` overwrites it at startup from resolveCloneCooldown (flag >
// config > default), and flow tests shrink it via t.Cleanup-restored
// assignment to drive the locked/unlocked branches without sleeping.
//
// Keyed by source conv, and applied only to agent-initiated clones:
// the runaway scenario the TODO flagged is "an agent cloning the same
// conv in a tight loop". Human-initiated clones are exempt (see
// isHumanCloneCaller) — a human can't loop at machine speed and clones
// deliberately. A manager agent that fans out clones of *different*
// sources hits the limit only if it tries the *same* source twice
// within cooldown.
var CloneCooldown = defaultCloneCooldown

// isHumanCloneCaller reports whether a clone was initiated by a human
// rather than an agent. Humans are exempt from CloneCooldown — the gate
// exists to bound a runaway *agent* loop, and a human can't fire clones
// at machine speed. A human reaches runCloneOrchestration with one of
// two caller shapes, both of which this must recognise:
//
//   - "": the /v1 endpoints, where requireCrossAgentPermission returns
//     "" for a classHuman peer (CLI or cookie-auth with no agent
//     ancestor).
//   - dashboardGranter ("<human-dashboard>"): the dashboard endpoint
//     (dashboardCloneAgent) records the human-dashboard sentinel as the
//     caller so the audit trail shows the human acted — but it is still
//     a human and must not be throttled.
//
// Both are empty / angle-bracketed pseudo-identities; an agent caller is
// always a real conv-id (a UUID), which never starts with "<". Keying
// the exemption here — rather than on a bare caller=="" — is what stops a
// dashboard clone from being wrongly treated as a runaway agent.
func isHumanCloneCaller(caller string) bool {
	return caller == "" || strings.HasPrefix(caller, "<")
}

// `tclaude agent clone` — fork the calling agent into a sibling that
// inherits its identity (groups, permissions, ownerships) but
// continues running independently. Unlike reincarnate, the original
// is NOT shut down and its identity rows are NOT removed.
//
// Two modes:
//
//   - default: copy the original's conv jsonl onto a fresh conv-id,
//     then spawn a new tclaude session with `-r <new-conv>`. The
//     clone starts with the SAME context as the original — useful for
//     "fork a worker to try a parallel approach."
//   - --no-copy-conv: skip the jsonl copy, spawn fresh CC. The clone
//     inherits identity only — useful for "stand up a peer in the
//     same role without dragging the conversation history along."
//
// Identity: the clone is renamed to `<original-title>-c-<N>` (or
// `c-<N>` if the original had no title) — the same `-c-` scheme
// `groups clone` uses, and the title sibling of reincarnate's `-r-`.
// The clone joins each of the original's groups, but membership rows
// carry no name of their own: the clone's single name is its title.

// cloneSuffixRegex matches a trailing clone suffix in either the
// current short form `-c-<digits>` or the legacy long form
// `-clone-<digits>`. Recognising both lets a legacy
// `worker-clone-3` cleanly transition to `worker-c-1` (rather than
// nesting as `worker-clone-3-c-1`) the next time it's cloned. Same
// idea for reincarnateSuffixRegex.
var cloneSuffixRegex = regexp.MustCompile(`^(.*?)-(?:c|clone)-\d+$`)

// uniqueCloneTitle computes the clone's conversation title. The format
// is ALWAYS `<base>-c-<N>` (or `c-<N>` when the original had no
// title). base is origTitle with any existing `-c-<digits>` /
// `-clone-<digits>` stripped, so a clone-of-a-clone bumps N rather
// than nesting suffixes (`worker-c-3` clones to `worker-c-4`, not
// `worker-c-3-c-1`). The short `-c-` keeps clone titles compact enough
// to tile in dashboard rows. (Reincarnation no longer has a parallel
// live-side suffix — post-JOH-319 the living generation keeps its plain
// base name; see agentd.retiredGenerationTitle.)
//
// N is monotonically larger than the previous clone's N: we start
// the search at `prevN + 1`, then advance to the smallest free slot
// from that floor. Without the floor, a previously-used N whose
// conv_index row has since disappeared (pruned, retitled, file
// deleted) gets recycled — chronologically confusing when the new
// clone descends from a higher-numbered ancestor. The "used" set
// scans every conv_index title so parallel clones don't collide;
// legacy `-clone-N` titles don't reserve a number in the new
// namespace.
//
// Lookup error → fall back to `prevN + 1` (or 1 when prevN is 0).
func uniqueCloneTitle(origTitle string) string {
	base := origTitle
	prevN := 0
	if m := cloneSuffixRegex.FindStringSubmatch(base); m != nil {
		base = m[1]
		// Re-extract N from the final dash-separated token; the regex
		// only captures the base. Same shape as the reincarnate
		// counterpart for symmetry.
		if i := strings.LastIndex(origTitle, "-"); i >= 0 {
			if n, err := strconv.Atoi(origTitle[i+1:]); err == nil {
				prevN = n
			}
		}
	}
	prefix := "c-"
	if base != "" {
		prefix = base + "-c-"
	}
	used := scanCloneSuffixes(prefix)
	start := max(prevN+1, 1)
	for n := start; ; n++ {
		if !used[n] {
			return prefix + strconv.Itoa(n)
		}
	}
}

// scanCloneSuffixes walks every conv_index row and returns the set of
// integers N where some custom_title equals `<prefix><N>`. Used by
// uniqueCloneTitle to pick the smallest free N.
func scanCloneSuffixes(prefix string) map[int]bool {
	used := map[int]bool{}
	rows, err := db.ListAllConvIndex()
	if err != nil {
		return used
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.CustomTitle, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(r.CustomTitle, prefix)
		n, err := strconv.Atoi(suffix)
		if err != nil {
			continue
		}
		used[n] = true
	}
	return used
}

// handleWhoamiClone handles POST /v1/whoami/clone (self path).
// Gated on self.clone (default-granted alongside self.compact /
// self.reincarnate). Delegates to runCloneOrchestration with
// target == caller.
func handleWhoamiClone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requirePermission(w, r, PermSelfClone)
	if !ok {
		return
	}
	if caller == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint clones the calling agent's own conversation; humans should use `tclaude conv copy` directly, or use POST /v1/agent/{conv}/clone to clone another agent")
		return
	}
	body, ok := decodeCloneBody(w, r)
	if !ok {
		return
	}
	runCloneOrchestration(w, caller, caller, PermSelfClone, body)
}

// handleAgentClone handles POST /v1/agent/{conv}/clone (cross-agent).
// Gated on agent.clone OR group-owner-of-target.
func handleAgentClone(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentClone, targetConv)
	if !ok {
		return
	}
	body, ok := decodeCloneBody(w, r)
	if !ok {
		return
	}
	runCloneOrchestration(w, targetConv, caller, PermAgentClone, body)
}

// cloneBody is the decoded, validated POST body shared by the clone
// endpoints (self / cross-agent / dashboard).
type cloneBody struct {
	// FollowUp is the optional first-turn prompt for the clone.
	FollowUp string `json:"follow_up"`
	// NoCopyConv spawns the clone with a fresh context instead of copying
	// the source's conversation jsonl.
	NoCopyConv bool `json:"no_copy_conv"`
	// Cwd is an optional override for where the clone's session is spawned —
	// empty means "inherit the source's cwd" (the historical behaviour); a
	// worktree path lets the human fork a clone onto a parallel branch.
	Cwd string `json:"cwd"`
	// WriteProofToken answers the dir write-proof challenge an agent caller
	// receives when it sets Cwd — same contract as SpawnRequest's field: the
	// caller must prove its own sandbox can write in the override directory
	// before it may aim a clone's write access there. Unused (and not
	// required) for humans and for clones that inherit the source's cwd.
	WriteProofToken string `json:"write_proof_token"`
}

// decodeCloneBody parses + validates the optional follow_up, no_copy_conv,
// cwd and write_proof_token body fields.
func decodeCloneBody(w http.ResponseWriter, r *http.Request) (cloneBody, bool) {
	var body cloneBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return cloneBody{}, false
		}
	}
	body.FollowUp = strings.TrimSpace(body.FollowUp)
	// Charset/length: validate against the inbox rule. Every clone
	// handoff — grouped or solo — rides the inbox as an agent_messages
	// row (the universal-inbox transport), so it tolerates the same
	// ≤16384-byte, newline-friendly charset as a spawn --initial-message.
	if body.FollowUp != "" && !isValidInitialMessage(body.FollowUp) {
		writeError(w, http.StatusBadRequest, "invalid_follow_up",
			fmt.Sprintf("REJECTED. follow_up must be at most %d characters; newlines "+
				"and tabs are allowed (a grouped clone receives the handoff in its "+
				"inbox, like a spawn brief), but NUL / escape / other control "+
				"characters are not.", agent.MaxInitialMessageBytes))
		return cloneBody{}, false
	}
	body.Cwd = strings.TrimSpace(body.Cwd)
	return body, true
}

// runCloneOrchestration is the target-agnostic body shared by self
// and cross-agent clone endpoints.
//
//   - target is the conv being cloned (its identity gets copied to the
//     new conv-id).
//   - caller is the conv that triggered the clone; recorded in the
//     audit trail (`system:clone:by=<caller>` for cross calls) and
//     used as the FromConv on the optional handoff message.
//   - perm is the slug requirePermission gated this call on
//     (PermSelfClone / PermAgentClone / "" for human dashboard). Used
//     to annotate `granted_by` with `:via-sudo:grant-id=<n>` when the
//     call only passed because of a sudo grant.
//   - body.Cwd, when non-empty, is the directory the clone's CC
//     session is spawned into instead of the source's cwd — typically
//     a git worktree path so a clone can pick up work on a parallel
//     branch. It's validated (exists, is a directory, "~" expanded)
//     before use; a bad value fails the whole clone with a 400. An
//     AGENT caller must additionally pass the dir write-proof for it
//     (see below).
func runCloneOrchestration(w http.ResponseWriter, target, caller, perm string, body cloneBody) {
	followUp, noCopyConv, cwdOverride := body.FollowUp, body.NoCopyConv, body.Cwd
	// 1. Snapshot target state. Same shape as reincarnate's snapshot
	// pass.
	oldSess := pickAliveSession(target)
	if oldSess == nil {
		writeError(w, http.StatusServiceUnavailable, "no_tmux",
			"target conv "+short8(target)+" has no live tmux session; can't clone without a cwd to spawn the sibling into")
		return
	}
	cwd := oldSess.Cwd
	var proofDirs []string
	var proofToken string
	_, profileWriteDirs, snapshotErr := effectiveSandboxWriteDirsForConv(target)
	if snapshotErr != nil {
		writeEffectiveSandboxLoadError(w, snapshotErr)
		return
	}
	srcHarness := harnessForConv(target).Name
	cloneSandbox := sandboxForHarness(srcHarness)
	codexGitCommonDir, gerr := spawnGitCommonDir(srcHarness, cloneSandbox, cwd)
	if gerr != nil {
		writeError(w, http.StatusInternalServerError, "io", gerr.Error())
		return
	}
	if cwdOverride != "" {
		resolved, err := resolveSpawnCwd(cwdOverride)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
			return
		}
		cwd = resolved
		codexGitCommonDir, gerr = spawnGitCommonDir(srcHarness, cloneSandbox, cwd)
		if gerr != nil {
			writeError(w, http.StatusInternalServerError, "io", gerr.Error())
			return
		}
	}
	var gitWriteDirs []string
	if spawnUsesPinnedGitCommonDir(srcHarness, cloneSandbox) {
		if home, err := os.UserHomeDir(); err == nil {
			gitWriteDirs = harness.GitWorktreeWriteDirs(cwd, codexGitCommonDir, home)
		}
	}
	// Every agent-triggered clone proves all authority the child receives,
	// including repository roots added by the managed sandbox. Inheriting cwd
	// alone is not an exemption: a newly installed profile may grant a broader
	// sibling-worktree container than the source session held.
	if !isHumanCloneCaller(caller) && (childSandboxGrantsDirWrite(srcHarness, cloneSandbox) || len(profileWriteDirs) > 0) {
		dirs := appendUniqueDirs([]string{cwd}, gitWriteDirs...)
		dirs = appendUniqueDirs(dirs, profileWriteDirs...)
		if len(dirs) > 0 {
			proofed, ok := requireDirWriteProof(w, caller, body.WriteProofToken, dirs)
			if !ok {
				return
			}
			if proofed != nil {
				proofToken = strings.TrimSpace(body.WriteProofToken)
				if v := proofed[cwd]; v != "" {
					cwd = v
				}
				for i, dir := range gitWriteDirs {
					if v := proofed[dir]; v != "" {
						gitWriteDirs[i] = v
					}
				}
				proofDirs = make([]string, 0, len(dirs))
				for _, raw := range dirs {
					proofDirs = appendUniqueDirs(proofDirs, proofed[raw])
				}
			}
		}
	}
	if proofToken != "" {
		defer cleanupDirWriteProofMarkers(proofToken, proofDirs)
	}

	// Snapshot group membership up-front — before the rate-limit claim
	// and the spawn. It decides how the handoff follow-up is delivered
	// (grouped → inbox, solo → send-keys) and therefore which charset
	// rule applies; doing it here means a follow-up the solo pane can't
	// carry is rejected without burning a rate-limit slot.
	oldGroups, err := db.ListGroupsForConv(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot groups: "+err.Error())
		return
	}
	oldMembers := make([]*db.AgentGroupMember, 0, len(oldGroups))
	for _, g := range oldGroups {
		m, err := db.FindMemberInGroup(g.ID, target)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io",
				"snapshot membership: "+err.Error())
			return
		}
		if m != nil {
			oldMembers = append(oldMembers, m)
		}
	}

	// Rate limit: refuse a second clone of the same source within
	// CloneCooldown — but only for agent-initiated clones. The gate
	// exists to bound a runaway *agent*: clone is the only default-
	// granted, agent-reachable fork-doubling verb (self.clone is
	// granted by default; reincarnate is 1-in-1-out, spawn is
	// human-only), so an agent stuck in a tight loop could fork itself
	// unboundedly. A human can't loop at machine speed and clones
	// deliberately, so human-initiated clones — CLI (caller == "") or
	// dashboard (caller == "<human-dashboard>") — skip the cooldown
	// entirely and don't even record a slot; isHumanCloneCaller spans
	// both shapes. Manager *agents* cloning peers via agent.clone still
	// have a real conv-id as caller and stay limited. Atomic at the DB
	// layer so two concurrent claim attempts can't both pass.
	if !isHumanCloneCaller(caller) {
		if err := db.ClaimCloneSlot(target, CloneCooldown, time.Now().UTC()); err != nil {
			if errors.Is(err, db.ErrCloneRateLimited) {
				writeError(w, http.StatusTooManyRequests, "rate_limited",
					"clone of "+short8(target)+" too recent; cooldown is "+CloneCooldown.String()+
						" between consecutive clones of the same source conv")
				return
			}
			writeError(w, http.StatusInternalServerError, "io",
				"clone rate-limit check: "+err.Error())
			return
		}
	}

	// Copy the full permission posture — grant AND deny overrides — so
	// the clone inherits the source's lockdown, not just its grants.
	oldPerms, err := db.ListAgentPermissionOverridesForConv(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot perms: "+err.Error())
		return
	}

	oldOwnedIDs, err := db.ListGroupsOwnedBy(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"snapshot ownerships: "+err.Error())
		return
	}

	// 2. Mint the clone's conv-id (and optionally its jsonl). The
	// branching logic + race-handling lives in cloneSpawnOnce so the
	// groups-clone orchestration can reuse the same code path without
	// duplicating it. The clone is launched with the source's live
	// model + effort (inheritedLaunchFlags; "" falls back to claude's
	// default) — a fork should run what the original runs.
	effort, model := inheritedLaunchFlags(oldSess.ID)
	newConv, newTmux, label, warn, spawnErr := cloneSpawnOnce(target, cwd, noCopyConv, effort, model, proofToken, proofToken != "", proofDirs, codexGitCommonDir, gitWriteDirs)
	if spawnErr != nil {
		spawnErr.write(w)
		return
	}

	// A clone is an agent in its own right. The identity copy below
	// registers it via the group/grant DB hooks when the original had
	// any, but a clone of a bare ungrouped agent would otherwise only
	// get an actor on its first /v1 call — make it explicit so it shows
	// on the roster the moment it spawns.
	// Stable agent-identity (JOH-26): a clone gets its OWN fresh agent_id here —
	// a clone is a FORK (no succession edge links it to the source), and newConv
	// is unlinked, so EnsureAgentForConv mints a new actor rather than inheriting
	// the source's.
	if _, _, err := db.EnsureAgentForConv(newConv, "clone"); err != nil {
		slog.Warn("clone: ensure new actor failed", "conv", newConv, "error", err)
	}
	if err := inheritEffectiveSandboxSnapshot(target, newConv); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "persist clone sandbox snapshot: "+err.Error())
		return
	}

	// 3. Copy identity to the new conv. Crucially, this is ADD-only —
	// the original keeps every membership / permission / ownership it
	// had. Best-effort per row; partial failure is recoverable via
	// the CLI.
	granter := "system:clone"
	if caller != target {
		granter = "system:clone:by=" + auditedCaller(caller, perm)
	} else if grantID, _ := db.LookupActiveSudoGrantID(caller, perm); grantID > 0 {
		// Self-clone via sudo: no :by= (it's just the target itself)
		// but still surface the via-sudo annotation so forensics can
		// tie the new conv's grants back to the elevation window.
		granter = fmt.Sprintf("system:clone:via-sudo:grant-id=%d", grantID)
	}
	// Resolve the original's display title so the clone's title can be
	// derived as `<base>-c-<N>`. Best-effort — an empty originalTitle
	// just means uniqueCloneTitle falls back to a bare `c-<N>`.
	// A non-CC harness (Codex) keeps its title in its own store
	// (threads.title); read it through the harness ConvStore so the clone
	// inherits the source's name. CC falls through to the conv_index path
	// unchanged.
	originalTitle := ""
	if t, ok := harnessNativeTitle(target); ok {
		originalTitle = t
	} else if row := agent.FreshConvRowResolved(target); row != nil {
		originalTitle = agent.DisplayTitle(row)
	}
	newTitle := uniqueCloneTitle(originalTitle)

	copied := applyClonedIdentity(newConv, granter, oldMembers, oldPerms, oldOwnedIDs)

	// 4. Optional follow-up. Same shape as reincarnate: enqueue an
	// agent_messages row and let the flush pipeline deliver it. A solo
	// (groupless) clone still gets a row — group_id 0 is a direct
	// message, the universal-inbox transport. FromConv is the caller
	// (original for self-clone, manager for cross-clone), so the new
	// clone sees who asked it to pick up work.
	//
	// Enqueued BEFORE the post-init goroutine fires (step 5) so that one
	// goroutine can deliver it — via flush, AFTER the /rename has settled.
	// The row must already exist when the flush runs.
	var msgID int64
	if followUp != "" {
		var handoffGroupID int64
		if len(oldMembers) > 0 {
			handoffGroupID = oldMembers[0].GroupID
		}
		id, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID:  handoffGroupID,
			FromConv: caller,
			ToConv:   newConv,
			Subject:  "clone handoff",
			Body:     followUp,
		})
		if err != nil {
			slog.Warn("clone: insert handoff message failed", "error", err)
		} else {
			msgID = id
		}
	}

	// 5. Single ordered post-init goroutine: wait-for-alive → /rename →
	// settle gap → flush the handoff nudge. The rename and the handoff
	// nudge MUST run in this order inside ONE goroutine. Previously they
	// were two racing goroutines (runClonePostInit + deliverHandoffViaFlush)
	// that both woke when the pane came alive and send-keys'd into the same
	// pane concurrently — so the nudge text landed inside the still-
	// unsubmitted /rename line, baking the nudge into the clone's title
	// (e.g. "worker-c-1[system: new agent message #N for you. ...]").
	// Mirrors reincarnate's runReincarnatePostSpawn.
	//
	// Renaming is done regardless of group membership — the clone has a
	// title whether or not it joined a group; without this startup write a
	// never-messaged clone ends up an orphan, the same trap that bit
	// `tclaude agent spawn` before bc7ec81.
	goBackground(func() {
		runClonePostInit(newConv, newTitle, target, caller)
	})

	// NB: no /exit on the original — that's the whole difference vs
	// reincarnate.

	resp := map[string]any{
		"old_conv":     target,
		"new_conv":     newConv,
		"label":        label,
		"tmux_session": newTmux,
		"copied":       copied,
		"copy_conv":    !noCopyConv,
	}
	if caller != target {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	if newTmux != "" && label != "" {
		resp["attach_cmd"] = "tclaude session attach " + label
	} else {
		resp["attach_cmd"] = "tclaude session resume " + newConv
	}
	if followUp != "" {
		resp["follow_up"] = followUp
		if msgID > 0 {
			resp["message_id"] = msgID
			resp["note"] = fmt.Sprintf("clone %s spawned alongside original %s; follow-up queued as message #%d",
				short8(newConv), short8(target), msgID)
		} else {
			resp["note"] = fmt.Sprintf("clone %s spawned alongside original %s; follow-up will be injected into the new pane once it's ready",
				short8(newConv), short8(target))
		}
	} else {
		resp["note"] = fmt.Sprintf("clone %s spawned alongside original %s; both are now running",
			short8(newConv), short8(target))
	}
	if warn != "" {
		resp["warning"] = warn
	}
	writeJSON(w, http.StatusOK, resp)
}

func inheritEffectiveSandboxSnapshot(sourceConv, targetConv string) error {
	var snapshot *sandboxpolicy.Snapshot
	if session, err := db.FindSessionByConvID(targetConv); err != nil {
		return err
	} else if session != nil {
		snapshot = session.EffectiveSandbox
	}
	var err error
	if snapshot == nil {
		snapshot, err = db.AgentEffectiveSandboxConfigForConv(sourceConv)
	}
	if err != nil || snapshot == nil {
		return err
	}
	agentID, _, err := db.EnsureAgentForConv(targetConv, "clone")
	if err != nil {
		return err
	}
	return db.SetAgentEffectiveSandboxConfig(agentID, snapshot)
}

// applyClonedIdentity copies a source agent's identity onto newConv: its group
// memberships, permission overrides (grants AND denies), and group ownerships.
// ADD-only — the source keeps everything it had. Best-effort per row (a failure
// is logged, not fatal); returns the descriptors copied (for the response /
// audit). Shared by the human/agent clone orchestration and the same-group
// export clone (JOH-266), which snapshot the source's identity and pass it here.
func applyClonedIdentity(newConv, granter string, members []*db.AgentGroupMember, perms map[string]string, ownedIDs []int64) []string {
	copied := []string{}
	for _, m := range members {
		// Membership rows carry no name of their own — the clone's single name
		// is its title, set by the caller's /rename.
		newMember := &db.AgentGroupMember{
			GroupID: m.GroupID,
			ConvID:  newConv,
			Role:    m.Role,
			Descr:   m.Descr,
		}
		if err := db.AddAgentGroupMember(newMember); err != nil {
			slog.Warn("clone: add new member failed", "group", m.GroupID, "error", err)
			continue
		}
		copied = append(copied, fmt.Sprintf("group:%d", m.GroupID))
	}

	for slug, effect := range perms {
		if err := db.SetAgentPermissionOverride(newConv, slug, effect, granter); err != nil {
			slog.Warn("clone: copy new perm failed", "slug", slug, "effect", effect, "error", err)
			continue
		}
		copied = append(copied, "perm:"+slug)
	}

	for _, gID := range ownedIDs {
		if err := db.AddAgentGroupOwner(gID, newConv, granter); err != nil {
			slog.Warn("clone: add new owner failed", "group", gID, "error", err)
			continue
		}
		copied = append(copied, fmt.Sprintf("owner:%d", gID))
	}
	return copied
}

// runClonePostInit fires asynchronously after a successful clone — as
// the SINGLE post-spawn goroutine, mirroring reincarnate's
// runReincarnatePostSpawn. It waits for the new pane to come online,
// injects /rename to the computed clone title (materialising the .jsonl
// with a meaningful name), then — after a settle gap — flushes any
// pending handoff/inbox nudges. Same purpose as runSpawnPostInit, just
// for the clone path: the original used to silently leave clones
// unrenamed (so they showed up as "(unknown)" with whatever conv-id-
// derived label tmux picked) and unrecoverable when never used.
//
// Why rename → gap → flush in ONE goroutine: the rename and the handoff
// nudge are both send-keys streams into the same pane. Running them
// concurrently (the old two-goroutine layout) let the nudge text land
// inside the still-unsubmitted /rename line, so the clone's title became
// "<base>-c-<N>[system: new agent message #N ...]". Serialising them
// with a settle gap keeps the rename a clean line of its own.
//
// Failures log; never bubble — the clone already succeeded as far as
// the caller is concerned.
func runClonePostInit(newConv, title, target, caller string) {
	if !waitForConvAlive(newConv) {
		slog.Warn("clone: new conv never came online; rename + handoff abandoned", "conv", newConv)
		return
	}
	// Rename first so the clone's CC title shows the proper
	// `<base>-c-<N>` before any handoff output streams. Skip only when
	// the title is empty or fails the rename charset gate.
	if title == "" || !isValidRenameTitle(title) {
		if title != "" {
			slog.Warn("clone: title not a valid rename title; skipping /rename",
				"conv", newConv, "title", title)
		}
	} else {
		if !deliverRename(newConv, title) {
			slog.Warn("clone: rename delivery failed", "conv", newConv, "title", title)
		}
		// Settle gap so CC processes the rename before the handoff
		// nudge's send-keys lands — without it the two keystroke streams
		// interleave into a single /rename line (the
		// "<base>-c-<N>[system: new agent message ...]" title bug).
		time.Sleep(reincarnateReadyDelay)
	}
	// Deliver any pending handoff / inbox nudges now that the rename has
	// settled. The orchestration enqueued the clone-handoff row (when a
	// follow-up was given) before launching this goroutine; the per-agent
	// dispatcher claims + delivers it through the normal nudge pipeline. No
	// synthetic welcome (unlike spawn) — the handoff row, when present, is
	// the clone's first prompt; the /rename alone already materialised the
	// .jsonl.
	enqueueDeliveryForConv(newConv)
}
