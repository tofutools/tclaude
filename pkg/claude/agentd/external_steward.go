package agentd

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// An external steward is an assistant the operator runs outside tclaude, acting
// on this instance alone or beside the built-in Steward. The process tree says
// nothing about it, so a credential the operator made proves who it is.
//
// The credential resolves to an ordinary agents row with a synthetic conv and
// no session, so the request classifies as classAgent and meets the gates every
// agent meets: there is no second authorization path, and requireHuman refuses
// it by construction. What differs only ever takes access away:
// externalStewardNever, no sudo, and no agent.default_permissions.

const (
	// externalStewardTokenPrefix marks a credential for secret scanners.
	externalStewardTokenPrefix = "tcls_"
	// externalStewardLevelGranter audits what a new principal starts with.
	externalStewardLevelGranter = "system:external-steward-levels"
	// externalStewardDefaultDays and externalStewardMaxDays bound a credential.
	externalStewardDefaultDays = 30
	externalStewardMaxDays     = 365
	// externalStewardTouchInterval limits last_used_at writes.
	externalStewardTouchInterval = time.Minute
	externalStewardAuthCode      = "external_steward_auth"
	externalStewardCeilingCode   = "external_steward_ceiling"
)

// externalStewardNever are the slugs this principal can never use, however it
// comes to hold them: a grant, a group, ownership, a popup. Each either answers
// the human's questions, hands out authority that would outlive the grant, or
// keeps acting after the credential ends.
// Listed explicitly rather than derived, so that adding a slug here is a
// decision; the flow test refuses every one.
var externalStewardNever = []string{
	// Answering the human's questions, directly or through a harness prompt
	// in a pane.
	PermApprovalsAnswer, PermHumanInboxRead, PermHumanInboxAnswer,
	PermSessionsAttach, PermSessionsTakeover, PermSessionsShell,
	// Handing out permissions, or the ownership and spawn settings that carry
	// them.
	PermPermissionsGrant, PermPermissionsRevoke, PermConfigImport, PermRolesManage,
	PermGroupsOwnersManage, PermGroupsSettingsMemberPermissions, PermGroupsSettingsOwnerScopes,
	PermGroupsMembersSpawnPermissions, PermGroupsMembersSpawnOwner, PermGroupsMembersSpawnAll,
	// Bundles that carry permissions or ownership to whoever is spawned from them.
	PermProfilesManage, PermTemplatesManage,
	// Receiving the operator's harness credentials.
	PermNodeCredentialsReceive,
	// Acting later without a credential: a job or trigger fires as the principal
	// after its credentials are revoked.
	PermSelfSchedule, PermAgentSchedule, PermGroupsMembersSchedule, PermGroupsMessagesSchedule,
	PermTriggersManage, PermGroupsTriggersManage,
	// Starting or changing a launch by any way but a pinned agent.spawn. A group
	// spawn comes with owning a group and names no sandbox, and a clone or morph
	// would copy a posture this principal could never set.
	PermGroupsMembersSpawn, PermAgentClone, PermGroupsMembersClone, PermAgentMorph, PermGroupsMembersMorph,
}

// externalStewardOptIn are level slugs a principal does not start with. A capture
// can show anything the operator has open, and it would leave for the assistant's
// own provider, so the operator grants it deliberately. Spawning has to name its
// groups and profiles, which only the operator can choose. See
// external_steward_spawn.go.
var externalStewardOptIn = []string{PermDashboardCapture, PermAgentSpawn}

func externalStewardRefuses(slug string) bool {
	return slices.Contains(externalStewardNever, slug)
}

// refuseExternalStewardCeiling stops a gate before any standing check, approval
// continuation or popup: not even the human can approve one of these for an
// external steward. Every gate that can open a popup calls it, and a test holds
// the list of those gates.
func refuseExternalStewardCeiling(w http.ResponseWriter, p *peer, perm string) bool {
	if !p.ExternalSteward || !externalStewardRefuses(perm) {
		return false
	}
	writeError(w, http.StatusForbidden, externalStewardCeilingCode, "an external steward can never use "+perm)
	return true
}

// externalStewardStartingSlugs is the Steward's Reads and Writes levels, less
// what stays opt-in here.
func externalStewardStartingSlugs(cfg *config.Config) []string {
	return slices.DeleteFunc(stewardLevelSlugs(cfg), func(slug string) bool {
		return slices.Contains(externalStewardOptIn, slug) || externalStewardRefuses(slug)
	})
}

// refuseExternalStewardGrant answers a grant the ceiling would ignore, so the
// operator is told instead of saving something that never takes effect. It
// returns the message, "" when the grant is fine.
func refuseExternalStewardGrant(convID string, slugs ...string) string {
	if !db.IsExternalStewardConv(convID) {
		return ""
	}
	for _, slug := range slugs {
		if externalStewardRefuses(slug) {
			return "an external steward can never hold " + slug + ": it answers the human or hands out authority, so the grant would do nothing"
		}
	}
	return ""
}

// refuseExternalStewardLaunch stops anything starting a harness for a principal:
// it has no conversation to resume, and it runs where the operator put it.
func refuseExternalStewardLaunch(w http.ResponseWriter, convID string) bool {
	if !db.IsExternalStewardConv(convID) {
		return false
	}
	writeError(w, http.StatusConflict, "external_steward", "an external steward runs outside tclaude, so tclaude cannot start it")
	return true
}

const externalStewardSudoRefusal = "an external steward cannot hold sudo; grant it a standing permission instead"

// mintExternalStewardCredential returns the credential, shown once, and the
// token id and hash that are stored. 32 random bytes need no slow hash.
func mintExternalStewardCredential() (credential, tokenID string, hash []byte, err error) {
	id := make([]byte, 8)
	secret := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return "", "", nil, err
	}
	if _, err := rand.Read(secret); err != nil {
		return "", "", nil, err
	}
	tokenID = hex.EncodeToString(id)
	encoded := base64.RawURLEncoding.EncodeToString(secret)
	sum := sha256.Sum256([]byte(encoded))
	return externalStewardTokenPrefix + tokenID + "_" + encoded, tokenID, sum[:], nil
}

// verifyExternalStewardCredential returns the credential's row, or a reason for
// the caller that says nothing specific unless the secret itself was right.
func verifyExternalStewardCredential(credential string, now time.Time) (auth *db.ExternalStewardAuth, reason string) {
	const refused = "external steward credential refused"
	rest, ok := strings.CutPrefix(credential, externalStewardTokenPrefix)
	if !ok {
		return nil, refused
	}
	tokenID, secret, ok := strings.Cut(rest, "_")
	if !ok || tokenID == "" || secret == "" {
		return nil, refused
	}
	row, err := db.LookupExternalStewardCredential(tokenID)
	if err != nil {
		slog.Warn("external steward: credential lookup failed", "error", err)
		return nil, refused
	}
	if row == nil {
		return nil, refused
	}
	sum := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(sum[:], row.Hash) != 1 {
		return nil, refused
	}
	switch {
	case !row.RevokedAt.IsZero():
		return nil, "this external steward credential was revoked"
	case !now.Before(row.ExpiresAt):
		return nil, "this external steward credential has expired; the operator can renew it"
	case row.AgentRetired:
		return nil, "this external steward was retired"
	}
	return row, ""
}

// externalDaemonPID is this daemon's pid. Swapped by tests, whose socket peer is
// the process running the daemon.
var externalDaemonPID = os.Getpid

// externalCallerUnderTclaude reports whether any ancestor is a process tclaude
// runs: the daemon, a recorded OpenCode server, or a pane on tclaude's own tmux
// server. That server is dedicated (-L tclaude), so any pane on it is something
// tclaude runs, one-shots included, which have no session row. OpenCode needs the
// other two: its server is a child of the daemon, so its tools sit under no pane,
// and it can outlive a daemon restart. Panes come from tmux because sessions.pid
// moves to the harness at the first hook, and a process that double-forked in a
// sandbox is a sibling of the harness, still under the pane through the sandbox's
// init. An orphan with no PID namespace reparents to host init and escapes this;
// the docs say so. Anything that cannot be answered counts as tclaude's, including
// a peer that has already exited, whose ancestry is gone: this check only ever
// refuses.
func externalCallerUnderTclaude(pid int) bool {
	out, err := tmuxOutputWithTimeout("list-panes", "-a", "-F", brokerPaneListFormat)
	if err != nil && !externalNoTmuxPanes(err) {
		return true
	}
	runtimes, err := db.ListOpenCodeRuntimes()
	if err != nil {
		return true
	}
	ours := map[int]bool{externalDaemonPID(): true}
	for _, runtime := range runtimes {
		ours[runtime.PID] = true
	}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 5 {
			continue
		}
		if panePID, err := strconv.Atoi(parts[2]); err == nil {
			ours[panePID] = true
		}
	}
	const maxHops = 256
	for range maxHops {
		if pid == 1 {
			return false
		}
		if ours[pid] {
			return true
		}
		if pid = procParent(pid); pid == 0 {
			return true
		}
	}
	return true
}

// externalNoTmuxPanes reports a failed pane listing that still answers the
// question: tmux missing, no server running, which is the normal state with no
// agent up, or a server kept alive with no sessions. Any other failure says
// nothing about panes that may be live, such as a client and server of different
// versions after an upgrade.
func externalNoTmuxPanes(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || tmuxServerNotRunning(err) ||
		(err != nil && strings.Contains(err.Error(), "no current target"))
}

// externalCallerManaged reports whether the socket peer is an agent tclaude
// runs. Such a caller keeps its own identity: a credential it read must not make
// it a larger one. An unmanaged harness is different from the operator token's
// rule on purpose: the token never promotes a harness descendant, because it
// would make it the human, while this credential only ever names a limited
// agent, and the assistant the operator chose may well be their own Claude Code.
// Session rows do not decide this: installed hooks can register a plain claude
// with some agent's tmux session. Swapped by tests.
var externalCallerManaged = func(pid int, claimedID string) bool {
	if claimedID != "" || externalCallerUnderTclaude(pid) {
		return true
	}
	_, _, selectedLayer := legacyIdentityForPID(pid)
	return selectedLayer
}

// authenticateExternalSteward turns a presented credential into the peer's
// identity, or writes the refusal. A credential was presented, so nothing falls
// through to the process tree or the operator token.
func authenticateExternalSteward(w http.ResponseWriter, r *http.Request, p *peer, claimedID, credential string) bool {
	if checkBrokerProofRate(r.URL.Path, "external-steward").Reject {
		writeError(w, http.StatusTooManyRequests, "rate", "too many identity proof attempts")
		return false
	}
	cfg, err := config.LoadShared()
	if err != nil || !cfg.CollabEnabled() || !cfg.ExternalStewardsEnabled() {
		writeError(w, http.StatusUnauthorized, externalStewardAuthCode, "external stewards are turned off on this instance")
		return false
	}
	now := time.Now()
	auth, reason := verifyExternalStewardCredential(credential, now)
	if auth == nil {
		writeError(w, http.StatusUnauthorized, externalStewardAuthCode, reason)
		return false
	}
	if externalCallerManaged(p.PID, claimedID) {
		writeError(w, http.StatusForbidden, externalStewardAuthCode, "a tclaude-managed agent cannot act as an external steward")
		return false
	}
	if err := db.TouchExternalStewardCredential(auth.TokenID, now, externalStewardTouchInterval); err != nil {
		slog.Warn("external steward: record use failed", "error", err)
	}
	p.ConvID, p.HasClaudeAncestor, p.ExternalSteward = auth.ConvID, true, true
	return true
}

var externalStewardName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

type externalStewardCredentialView struct {
	TokenID    string     `json:"token_id"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  string     `json:"created_by"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Live       bool       `json:"live"`
}

type externalStewardView struct {
	AgentID     string                          `json:"agent_id"`
	Name        string                          `json:"name"`
	Retired     bool                            `json:"retired,omitempty"`
	Credentials []externalStewardCredentialView `json:"credentials"`
	Spawn       *externalStewardSpawnPin        `json:"spawn,omitempty"`
	Children    []externalStewardSpawnChild     `json:"children,omitempty"`
}

func externalStewardTimeView(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func externalStewardViewOf(s db.ExternalSteward, now time.Time) externalStewardView {
	view := externalStewardView{AgentID: s.AgentID, Name: s.Name, Retired: s.Retired, Credentials: []externalStewardCredentialView{}}
	for _, c := range s.Credentials {
		view.Credentials = append(view.Credentials, externalStewardCredentialView{
			TokenID: c.TokenID, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, ExpiresAt: c.ExpiresAt,
			RevokedAt: externalStewardTimeView(c.RevokedAt), LastUsedAt: externalStewardTimeView(c.LastUsedAt),
			Live: c.Live(now) && !s.Retired,
		})
	}
	var err error
	if view.Spawn, err = externalStewardSpawnPinOf(s.ConvID); err != nil {
		slog.Warn("external steward: read its spawn pin failed", "name", s.Name, "error", err)
	}
	if view.Children, err = externalStewardChildren(s.AgentID); err != nil {
		slog.Warn("external steward: list its workers failed", "name", s.Name, "error", err)
	}
	return view
}

// externalStewardIssued is the only place a credential is ever returned.
type externalStewardIssued struct {
	AgentID    string    `json:"agent_id"`
	Name       string    `json:"name"`
	TokenID    string    `json:"token_id"`
	ExpiresAt  time.Time `json:"expires_at"`
	Credential string    `json:"credential"`
	Grants     []string  `json:"grants,omitempty"`
}

// externalStewardIdentity names a principal for what the operator sees. Its conv
// has no conversation row for a selector to find.
func externalStewardIdentity(convID string) (agentID, name string) {
	stewards, err := db.ListExternalStewards()
	if err != nil {
		slog.Warn("external steward: list for its name failed", "error", err)
		return "", ""
	}
	for _, steward := range stewards {
		if steward.ConvID == convID {
			return steward.AgentID, guideText(steward.Name, 80)
		}
	}
	return "", ""
}

// soleExternalSteward finds who the address "steward" falls to when no built-in
// Steward runs. Only a principal that can authenticate counts, since mail to any
// other would wait for nobody. able says how many are, and address is usable only
// when that is one. It is an agent id: the conv has no conversation row for a
// selector to find.
func soleExternalSteward() (address string, able int) {
	cfg, err := config.LoadShared()
	if err != nil || !cfg.ExternalStewardsEnabled() {
		return "", 0
	}
	stewards, err := db.ListExternalStewards()
	if err != nil {
		slog.Warn("external steward: list for the steward address failed", "error", err)
		return "", 0
	}
	now := time.Now()
	for _, steward := range stewards {
		if !steward.Retired && slices.ContainsFunc(steward.Credentials, func(c db.ExternalStewardCredential) bool {
			return c.Live(now)
		}) {
			address = steward.AgentID
			able++
		}
	}
	return address, able
}

func externalStewardExpiry(w http.ResponseWriter, days int) (time.Time, bool) {
	if days == 0 {
		days = externalStewardDefaultDays
	}
	if days < 1 || days > externalStewardMaxDays {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a credential lasts 1 to 365 days")
		return time.Time{}, false
	}
	return time.Now().AddDate(0, 0, days), true
}

// externalStewardOn reports the switch, writing the refusal when it is off.
func externalStewardOn(w http.ResponseWriter) (*config.Config, bool) {
	cfg, ok := stewardRequestConfig(w)
	if !ok {
		return nil, false
	}
	if !cfg.ExternalStewardsEnabled() {
		writeError(w, http.StatusConflict, "external_stewards_off",
			"external stewards are off; set steward.external_enabled in the config first")
		return nil, false
	}
	return cfg, true
}

// GET /v1/external-stewards: every principal and its credentials, never a hash.
func handleExternalStewardsList(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "list external stewards") {
		return
	}
	stewards, err := db.ListExternalStewards()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	now := time.Now()
	out := make([]externalStewardView, 0, len(stewards))
	for _, s := range stewards {
		out = append(out, externalStewardViewOf(s, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"external_stewards": out, "spawn_choices": externalStewardSpawnChoices()})
}

// POST /v1/external-stewards: a new principal and its first credential.
func handleExternalStewardCreate(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "create an external steward") {
		return
	}
	cfg, ok := externalStewardOn(w)
	if !ok {
		return
	}
	var body struct {
		Name        string `json:"name"`
		ExpiresDays int    `json:"expires_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if !externalStewardName.MatchString(body.Name) || isStewardAddress(body.Name) {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"a name is lowercase letters, digits and dashes, up to 40 characters, and not \"steward\"")
		return
	}
	expires, ok := externalStewardExpiry(w, body.ExpiresDays)
	if !ok {
		return
	}
	credential, tokenID, hash, err := mintExternalStewardCredential()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	grants := externalStewardStartingSlugs(cfg)
	agentID, err := db.CreateExternalSteward(body.Name, db.ExternalStewardConvPrefix+convops.GenerateUUID(), "human",
		tokenID, hash, expires, grants, externalStewardLevelGranter)
	if errors.Is(err, db.ErrExternalStewardNameTaken) {
		writeError(w, http.StatusConflict, "name_taken", "an external steward already has that name")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, externalStewardIssued{
		AgentID: agentID, Name: body.Name, TokenID: tokenID, ExpiresAt: expires, Credential: credential, Grants: grants,
	})
}

func externalStewardNamed(w http.ResponseWriter, r *http.Request) (*db.ExternalSteward, bool) {
	s, err := db.GetExternalStewardByName(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return nil, false
	}
	if s == nil {
		writeError(w, http.StatusNotFound, "not_found", "no external steward has that name")
		return nil, false
	}
	return s, true
}

// POST /v1/external-stewards/{name}/renew: a new credential, shown once. The old
// ones keep working unless revoke_existing says otherwise, so an assistant can
// be moved over before they end.
func handleExternalStewardRenew(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "renew an external steward") {
		return
	}
	if _, ok := externalStewardOn(w); !ok {
		return
	}
	s, ok := externalStewardNamed(w, r)
	if !ok {
		return
	}
	if s.Retired {
		writeError(w, http.StatusConflict, "retired", "this external steward was retired; create a new one")
		return
	}
	var body struct {
		ExpiresDays    int  `json:"expires_days"`
		RevokeExisting bool `json:"revoke_existing"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	expires, ok := externalStewardExpiry(w, body.ExpiresDays)
	if !ok {
		return
	}
	credential, tokenID, hash, err := mintExternalStewardCredential()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if err := db.AddExternalStewardCredential(s.AgentID, "human", tokenID, hash, expires, body.RevokeExisting); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, externalStewardIssued{
		AgentID: s.AgentID, Name: s.Name, TokenID: tokenID, ExpiresAt: expires, Credential: credential,
	})
}

// POST /v1/external-stewards/{name}/revoke: end one credential by token_id, or
// all of them. It works with the switch off, so access can always be taken away.
func handleExternalStewardRevoke(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "revoke an external steward") {
		return
	}
	s, ok := externalStewardNamed(w, r)
	if !ok {
		return
	}
	var body struct {
		TokenID      string `json:"token_id"`
		StopChildren bool   `json:"stop_children"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	revoked, err := db.RevokeExternalStewardCredentials(s.AgentID, body.TokenID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// Workers keep running unless the operator says otherwise: stopping one
	// mid-edit does damage, and at the baseline posture it can't act without them.
	children, err := externalStewardChildren(s.AgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	var stopped []memberOpResult
	if body.StopChildren {
		for _, child := range children {
			if child.Online {
				stopped = append(stopped, stopOneConvWithIntent(child.ConvID, false, db.AgentExitActionStop, auditRequestEventID(r)))
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": revoked, "children": children, "stopped": stopped})
}

// takeExternalStewardCredential reads the presented credential and removes it
// from the request, so no handler below can log it or forward it upstream.
func takeExternalStewardCredential(r *http.Request) string {
	credential := strings.TrimSpace(r.Header.Get(agentipc.ExternalStewardCredentialHeader))
	r.Header.Del(agentipc.ExternalStewardCredentialHeader)
	return credential
}
