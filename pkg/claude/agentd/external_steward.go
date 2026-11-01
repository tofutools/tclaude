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
	"regexp"
	"slices"
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
// the human's questions or hands out authority that would outlive the grant.
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
	// Receiving the operator's harness credentials.
	PermNodeCredentialsReceive,
}

// externalStewardOptIn are level slugs a principal does not start with: a
// capture can show anything the operator has open, and it would leave for the
// assistant's own provider. The operator grants it deliberately.
var externalStewardOptIn = []string{PermDashboardCapture}

func externalStewardRefuses(slug string) bool {
	return slices.Contains(externalStewardNever, slug)
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

// externalCallerManaged reports whether the socket peer is an agent tclaude
// runs. Such a caller keeps its own identity: a credential it read must not make
// it a larger one. An unmanaged harness is different from the operator token's
// rule on purpose: the token never promotes a harness descendant, because it
// would make it the human, while this credential only ever names a limited
// agent, and the assistant the operator chose may well be their own Claude Code.
// Swapped by tests.
var externalCallerManaged = func(pid int, claimedID string) bool {
	if claimedID != "" {
		return true
	}
	convID, _, selectedLayer := legacyIdentityForPID(pid)
	if selectedLayer {
		return true
	}
	if convID == "" {
		return false
	}
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(rows, func(row *db.SessionRow) bool { return row.TmuxSession != "" })
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
	writeJSON(w, http.StatusOK, map[string]any{"external_stewards": out})
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
		TokenID string `json:"token_id"`
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
	writeJSON(w, http.StatusOK, map[string]any{"revoked": revoked})
}

// externalStewardCredentialFrom reads the presented credential.
func externalStewardCredentialFrom(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(agentipc.ExternalStewardCredentialHeader))
}
