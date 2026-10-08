package agentd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const fedEnrollmentTTL = 2 * time.Minute

type fedEnrollmentPending struct {
	master []byte
	token  *proto.EnrollmentToken
	result chan proto.EnrollmentResult
}

// Enrollment is the only bootstrap exception to trusted-peer admission. Both
// kinds are small, signed and sealed; results additionally require a live nonce
// and the master key pinned by the operator's bearer. Nothing enters the outbox.
func (rt *fedRuntime) handleEnrollmentInbound(from string, s *proto.Sealed) bool {
	if s == nil {
		return false
	}
	var header struct {
		Kind      string `json:"kind"`
		InReplyTo string `json:"in_reply_to"`
	}
	if len(s.Env) > proto.MaxEnrollmentBytes {
		return false
	}
	if json.Unmarshal(s.Env, &header) != nil || (header.Kind != proto.KindEnrollRequest && header.Kind != proto.KindEnrollResult) {
		return false
	}
	var key []byte
	var pending fedEnrollmentPending
	if header.Kind == proto.KindEnrollResult {
		rt.enrollmentMu.Lock()
		pending = rt.enrollmentPending[header.InReplyTo]
		rt.enrollmentMu.Unlock()
		if pending.result == nil || proto.InstanceID(pending.master) != from {
			return true
		}
		key = pending.master
	} else {
		// Bound work before signature/decryption and keep the per-key map bounded.
		if !rt.enrollmentAdmit(from) {
			return true
		}
		for _, p := range rt.cl.Directory() {
			if p.InstanceID == from && proto.InstanceID(p.PubKey) == from {
				key = p.PubKey
				break
			}
		}
		if len(key) != ed25519.PublicKeySize {
			return true
		}
	}
	env, e := proto.Open(s, ed25519.PublicKey(key), rt.id, time.Now())
	if e != nil || env.From.Instance != from || env.From.Agent != "" || env.To.Agent != "" || env.ExpiresAt.After(time.Now().Add(fedEnrollmentTTL+10*time.Second)) || env.CreatedAt.After(time.Now().Add(10*time.Second)) {
		return true
	}
	if header.Kind == proto.KindEnrollResult {
		var result proto.EnrollmentResult
		if env.DecodePayload(&result) != nil {
			return true
		}
		c := pending.token.Claims
		if result.TokenID != c.TokenID || result.Node != rt.id.ID() || result.ProfileID != c.ProfileID || result.ProfileRevision != c.ProfileRevision || result.TrustLevel != c.TrustLevel {
			return true
		}
		select {
		case pending.result <- result:
		default:
		}
		return true
	}
	var req proto.EnrollmentRequest
	if env.InReplyTo != "" || env.DecodePayload(&req) != nil {
		return true
	}
	token, e := proto.ParseEnrollmentToken(req.Token)
	if e != nil || !bytes.Equal(token.Claims.MasterKey, rt.id.Pub) {
		return true
	}
	c := token.Claims
	result := proto.EnrollmentResult{TokenID: c.TokenID, Node: from, ProfileID: c.ProfileID, ProfileRevision: c.ProfileRevision, TrustLevel: c.TrustLevel, Code: "enrollment_refused"}
	// Inbound workers must never wait on lifecycle locks during shutdown:
	// stop holds the lifecycle lock while joining these workers.
	if !rt.lockEnrollmentMutation() {
		return true
	}
	var created bool
	if currentFederation() != rt {
		e = errors.New("federation changed")
	} else {
		created, e = db.RedeemFederationEnrollment(token, key, rt.id.Pub)
	}
	fedLifecycleMu.Unlock()
	fedNodeGroupsMu.Unlock()
	result.Accepted = e == nil
	if result.Accepted {
		result.Code = ""
	}
	if created && e == nil {
		recordFederationAudit("federation.enroll.accept", from, "", "", c.TokenID, 200)
		broadcastFederationCatalogs()
	}
	_ = rt.sendEnrollment(key, proto.KindEnrollResult, env.ID, result)
	return true
}
func (rt *fedRuntime) lockEnrollmentMutation() bool {
	for {
		if fedNodeGroupsMu.TryLock() {
			if fedLifecycleMu.TryLock() {
				return true
			}
			fedNodeGroupsMu.Unlock()
		}
		select {
		case <-rt.ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func (rt *fedRuntime) enrollmentAdmit(from string) bool {
	rt.enrollmentMu.Lock()
	defer rt.enrollmentMu.Unlock()
	now := time.Now()
	cut := now.Add(-time.Minute)
	prune := func(v []time.Time) []time.Time {
		j := 0
		for _, t := range v {
			if t.After(cut) {
				v[j] = t
				j++
			}
		}
		return v[:j]
	}
	rt.enrollmentGlobal = prune(rt.enrollmentGlobal)
	if len(rt.enrollmentGlobal) >= 60 {
		return false
	}
	if rt.enrollmentRates == nil {
		rt.enrollmentRates = map[string][]time.Time{}
	}
	for k, v := range rt.enrollmentRates {
		v = prune(v)
		if len(v) == 0 {
			delete(rt.enrollmentRates, k)
		} else {
			rt.enrollmentRates[k] = v
		}
	}
	v := rt.enrollmentRates[from]
	if len(v) >= 6 {
		return false
	}
	rt.enrollmentRates[from] = append(v, now)
	rt.enrollmentGlobal = append(rt.enrollmentGlobal, now)
	return true
}
func (rt *fedRuntime) sendEnrollment(key []byte, kind, reply string, payload any) error {
	env, e := proto.NewEnvelope(rt.id, kind, proto.Endpoint{}, proto.Endpoint{Instance: proto.InstanceID(key)}, fedEnrollmentTTL, payload)
	if e != nil {
		return e
	}
	env.InReplyTo = reply
	sealed, e := proto.Seal(rt.id, env, ed25519.PublicKey(key))
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(rt.ctx, 15*time.Second)
	defer cancel()
	_, e = rt.cl.Send(ctx, env.To.Instance, sealed)
	return e
}
func registerFederationEnrollmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/federation/enroll-tokens", handleFederationEnrollmentTokens)
	mux.HandleFunc("POST /v1/federation/enroll-tokens", handleFederationEnrollmentTokens)
	mux.HandleFunc("POST /v1/federation/enroll-tokens/{id}/revoke", handleFederationEnrollmentTokens)
	mux.HandleFunc("GET /v1/federation/enrollments", handleFederationEnrollments)
	mux.HandleFunc("POST /v1/federation/enroll/preview", handleFederationEnroll)
	mux.HandleFunc("POST /v1/federation/enroll", handleFederationEnroll)
}
func handleFederationEnrollments(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "list enrolled nodes") {
		return
	}
	out, e := db.ListFederationEnrollments()
	if e != nil {
		writeFedErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"enrollments": out})
}
func handleFederationEnrollmentTokens(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage enrollment tokens") {
		return
	}
	if r.Method == http.MethodGet {
		out, e := db.ListFederationEnrollmentTokens()
		if e != nil {
			writeFedErr(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"tokens": out})
		return
	}
	if id := r.PathValue("id"); id != "" {
		if e := db.RevokeFederationEnrollmentToken(id); e != nil {
			writeError(w, 404, "token", "unknown token")
			return
		}
		recordFederationAudit("federation.enroll.revoke", "operator", "", "", id, 200)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	var in struct {
		Profile    string `json:"profile"`
		Uses       int    `json:"uses"`
		TTLSeconds int64  `json:"ttl_seconds"`
		TrustLevel string `json:"trust_level"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
		writeError(w, 400, "json", "invalid enrollment options")
		return
	}
	if in.Uses == 0 {
		in.Uses = 1
	}
	if in.TTLSeconds == 0 {
		in.TTLSeconds = 86400
	}
	if in.TrustLevel == "" {
		in.TrustLevel = db.FederationTrustRestricted
	}
	if in.Uses < 1 || in.Uses > 10000 || in.TTLSeconds < 1 || in.TTLSeconds > 30*86400 || (in.TrustLevel != db.FederationTrustRestricted && in.TrustLevel != db.FederationTrustUnrestricted) {
		writeError(w, 400, "options", "uses must be 1-10000, TTL 1s-30d and trust level restricted or unrestricted")
		return
	}
	fedNodeGroupsMu.Lock()
	defer fedNodeGroupsMu.Unlock()
	p, e := db.GetFederationNodeProfile(in.Profile)
	if e != nil {
		writeError(w, 404, "profile", "unknown profile")
		return
	}
	id, e := federationIdentity()
	if e != nil {
		writeFedErr(w, e)
		return
	}
	bearer, token, e := proto.NewEnrollmentToken(id, p.ID, p.Name, p.Revision, in.TrustLevel, time.Now().Add(time.Duration(in.TTLSeconds)*time.Second))
	if e == nil {
		e = db.SaveFederationEnrollmentToken(token, in.Uses)
	}
	if e != nil {
		writeError(w, 409, "enrollment", "could not create token")
		return
	}
	recordFederationAudit("federation.enroll.create", "operator", "", "", token.Claims.TokenID, 200)
	writeJSON(w, 200, map[string]any{"token": bearer, "claims": token.Claims, "master_fingerprint": proto.Fingerprint(id.Pub), "uses": in.Uses})
}

type fedEnrollmentInput struct {
	Master  string `json:"master"`
	Token   string `json:"token"`
	Preview string `json:"preview_token"`
}

func enrollmentPreview(rt *fedRuntime, in fedEnrollmentInput) (*proto.EnrollmentToken, string, error) {
	t, e := proto.ParseEnrollmentToken(in.Token)
	if e != nil {
		return nil, "", e
	}
	if t.Claims.Master == rt.id.ID() {
		return nil, "", errors.New("cannot enroll to this node itself")
	}
	found := false
	// Names are selectors only: a matching name must still have the signed key.
	for _, p := range rt.cl.Directory() {
		if p.InstanceID == t.Claims.Master && bytes.Equal(p.PubKey, t.Claims.MasterKey) && (in.Master == p.InstanceID || in.Master == p.Name) {
			found = true
		}
	}
	if !found {
		return nil, "", errors.New("master is unavailable or selector does not match token identity; use the signed master ID")
	}
	if !time.Now().Before(t.Claims.ExpiresAt) {
		return nil, "", errors.New("enrollment token expired")
	}
	preview, e := db.FederationEnrollmentNodePreview(t)
	return t, preview, e
}
func handleFederationEnroll(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "enroll this node") {
		return
	}
	var in fedEnrollmentInput
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in) != nil {
		writeError(w, 400, "json", "invalid enrollment request")
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 409, "federation", "federation must be connected to the hub first")
		return
	}
	t, preview, e := enrollmentPreview(rt, in)
	if e != nil {
		writeError(w, 409, "enrollment", e.Error())
		return
	}
	if strings.HasSuffix(r.URL.Path, "/preview") {
		writeJSON(w, 200, map[string]any{"claims": t.Claims, "preview_token": preview, "master_fingerprint": proto.Fingerprint(ed25519.PublicKey(t.Claims.MasterKey)), "node_fingerprint": proto.Fingerprint(rt.id.Pub), "consent": "Running enroll trusts the pinned master at the displayed level. Its profile controls this node's authority on the master. No default profile or config offer is applied locally."})
		return
	}
	if in.Preview == "" || in.Preview != preview {
		writeError(w, 409, "stale_preview", "preview enrollment again")
		return
	}
	env, e := proto.NewEnvelope(rt.id, proto.KindEnrollRequest, proto.Endpoint{}, proto.Endpoint{Instance: t.Claims.Master}, fedEnrollmentTTL, proto.EnrollmentRequest{Token: in.Token})
	if e != nil {
		writeError(w, 500, "enrollment", "could not prepare enrollment")
		return
	}
	sealed, e := proto.Seal(rt.id, env, ed25519.PublicKey(t.Claims.MasterKey))
	if e != nil {
		writeError(w, 500, "enrollment", "could not seal enrollment")
		return
	}
	ch := make(chan proto.EnrollmentResult, 1)
	rt.enrollmentMu.Lock()
	if rt.enrollmentPending == nil {
		rt.enrollmentPending = map[string]fedEnrollmentPending{}
	}
	if len(rt.enrollmentPending) >= 64 {
		rt.enrollmentMu.Unlock()
		writeError(w, 429, "enrollment", "too many pending enrollments")
		return
	}
	rt.enrollmentPending[env.ID] = fedEnrollmentPending{master: t.Claims.MasterKey, token: t, result: ch}
	rt.enrollmentMu.Unlock()
	defer func() { rt.enrollmentMu.Lock(); delete(rt.enrollmentPending, env.ID); rt.enrollmentMu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, e = rt.cl.Send(ctx, t.Claims.Master, sealed); e != nil {
		writeError(w, 502, "enrollment", "master unavailable; retry with the same token")
		return
	}
	select {
	case result := <-ch:
		if !result.Accepted {
			writeError(w, 409, "enrollment_refused", "master refused enrollment; token may be exhausted, revoked, changed, or this key retired")
			return
		}
		fedNodeGroupsMu.Lock()
		finish := lockAwayMutation(t.Claims.Master, true)
		// An old runtime cannot install trust after key rotation/reload.
		if currentFederation() != rt {
			e = errors.New("federation changed; retry enrollment")
		} else {
			e = db.CompleteFederationEnrollment(t, rt.id.Pub, in.Preview)
		}
		finish()
		fedNodeGroupsMu.Unlock()
		if e != nil {
			writeError(w, 409, "enrollment", e.Error())
			return
		}
		recordFederationAudit("federation.enroll.complete", t.Claims.Master, "", "", t.Claims.TokenID, 200)
		broadcastFederationCatalogs()
		// The master's first catalog may have arrived before its key was trusted.
		go rt.sendControl(t.Claims.Master, proto.KindCatalogReq, "", nil)
		writeJSON(w, 200, result)
	case <-ctx.Done():
		writeError(w, 504, "enrollment_timeout", "no confirmation; retry with the same token")
	case <-rt.ctx.Done():
		writeError(w, 409, "enrollment", "federation changed; retry enrollment")
	}
}
