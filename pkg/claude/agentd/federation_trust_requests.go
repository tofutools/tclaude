package agentd

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Trust requests let an operator ask a hub-visible instance to trust their
// node, or to trust it back. They live in memory only: a request is a nudge
// for the receiving operator, never state that grants anything, and the
// sender can simply ask again after a restart.
const (
	fedTrustRequestTTL      = 7 * 24 * time.Hour
	fedTrustRequestMax      = 64
	fedTrustRequestCooldown = time.Minute
)

type fedTrustRequest struct {
	Note       string    `json:"note,omitempty"`
	TrustsYou  bool      `json:"trusts_you,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
}

type fedTrustRequestState struct {
	in     map[string]fedTrustRequest
	out    map[string]time.Time
	// dismissed holds when each sender's request was dismissed, so a replay
	// of that same envelope (created before) does not bring it back.
	dismissed map[string]time.Time
	rates  map[string][]time.Time
	global []time.Time
}

// cleanTrustNote keeps a single printable line of at most
// MaxTrustRequestNote runes; ok is false when the note is too long.
func cleanTrustNote(s string) (string, bool) {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return s, len([]rune(s)) <= proto.MaxTrustRequestNote
}

// trustRequestAdmit bounds inbound work per sender and overall, like
// enrollmentAdmit, before any signature or decryption.
func (rt *fedRuntime) trustRequestAdmit(from string, now time.Time) bool {
	st := &rt.trustReq
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
	st.global = prune(st.global)
	if len(st.global) >= 30 {
		return false
	}
	if st.rates == nil {
		st.rates = map[string][]time.Time{}
	}
	for k, v := range st.rates {
		if v = prune(v); len(v) == 0 {
			delete(st.rates, k)
		} else {
			st.rates[k] = v
		}
	}
	if len(st.rates[from]) >= 3 {
		return false
	}
	st.rates[from] = append(st.rates[from], now)
	st.global = append(st.global, now)
	return true
}

// handleTrustRequestInbound takes a trust request from any directory peer.
// It reports whether the envelope was one (handled or dropped), so the
// trusted-peer path never sees it.
func (rt *fedRuntime) handleTrustRequestInbound(from string, s *proto.Sealed) bool {
	if s == nil {
		return false
	}
	// Larger envelopes are never trust requests: leave them to the
	// trusted-peer path without parsing them here.
	if len(s.Env) > proto.MaxTrustRequestBytes {
		return false
	}
	var header struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(s.Env, &header) != nil || header.Kind != proto.KindTrustRequest {
		return false
	}
	now := time.Now()
	rt.trustReqMu.Lock()
	ok := rt.trustRequestAdmit(from, now)
	rt.trustReqMu.Unlock()
	if !ok {
		return true
	}
	// A trusted sender has nothing to ask for.
	if p, err := db.GetFederationPeer(from); err != nil || p != nil {
		return true
	}
	key := rt.directoryKey(from)
	if key == nil {
		return true
	}
	env, err := proto.Open(s, ed25519.PublicKey(key), rt.id, now)
	if err != nil || env.From.Instance != from || env.From.Agent != "" || env.To.Agent != "" || env.InReplyTo != "" ||
		env.ExpiresAt.After(now.Add(fedEnrollmentTTL+10*time.Second)) || env.CreatedAt.After(now.Add(10*time.Second)) {
		return true
	}
	var req proto.TrustRequest
	if env.DecodePayload(&req) != nil {
		return true
	}
	note, fits := cleanTrustNote(req.Note)
	if !fits {
		return true
	}
	rt.trustReqMu.Lock()
	st := &rt.trustReq
	if at, ok := st.dismissed[from]; ok && !env.CreatedAt.After(at) {
		rt.trustReqMu.Unlock()
		return true
	}
	if st.in == nil {
		st.in = map[string]fedTrustRequest{}
	}
	st.pruneIn(now)
	if _, known := st.in[from]; !known && len(st.in) >= fedTrustRequestMax {
		oldest := ""
		for id, r := range st.in {
			if oldest == "" || r.ReceivedAt.Before(st.in[oldest].ReceivedAt) {
				oldest = id
			}
		}
		delete(st.in, oldest)
	}
	st.in[from] = fedTrustRequest{Note: note, TrustsYou: req.TrustsYou, ReceivedAt: now}
	rt.trustReqMu.Unlock()
	recordFederationAudit("federation.trust_request.receive", from, "", "", "", 200)
	return true
}

func (st *fedTrustRequestState) pruneIn(now time.Time) {
	for id, r := range st.in {
		if now.Sub(r.ReceivedAt) > fedTrustRequestTTL {
			delete(st.in, id)
		}
	}
	// A replay is only possible within an envelope's lifetime.
	for id, at := range st.dismissed {
		if now.Sub(at) > fedEnrollmentTTL+time.Minute {
			delete(st.dismissed, id)
		}
	}
}

// forgetTrustRequest drops a request once its sender is trusted, so an
// untrust later does not bring the old request back.
func (rt *fedRuntime) forgetTrustRequest(id string) {
	rt.trustReqMu.Lock()
	defer rt.trustReqMu.Unlock()
	delete(rt.trustReq.in, id)
}

// directoryKey is a directory peer's signing key, only when the hub-listed
// key matches the instance ID it claims.
func (rt *fedRuntime) directoryKey(id string) []byte {
	for _, p := range rt.cl.Directory() {
		if p.InstanceID == id && proto.InstanceID(p.PubKey) == id && len(p.PubKey) == ed25519.PublicKeySize {
			return p.PubKey
		}
	}
	return nil
}

// trustRequestView is what the status read attaches to an untrusted row: a
// request received from it, and when this node last asked it.
func (rt *fedRuntime) trustRequestView() (map[string]fedTrustRequest, map[string]time.Time) {
	rt.trustReqMu.Lock()
	defer rt.trustReqMu.Unlock()
	now := time.Now()
	rt.trustReq.pruneIn(now)
	in := make(map[string]fedTrustRequest, len(rt.trustReq.in))
	for id, r := range rt.trustReq.in {
		in[id] = r
	}
	out := make(map[string]time.Time, len(rt.trustReq.out))
	for id, at := range rt.trustReq.out {
		if now.Sub(at) <= fedTrustRequestTTL {
			out[id] = at
		}
	}
	return in, out
}

type fedTrustRequestReq struct {
	Instance string `json:"instance"`
	Note     string `json:"note,omitempty"`
}

// handleFederationTrustRequest sends a trust request to a hub-visible
// instance (POST), or dismisses one received from it (POST …/dismiss).
func handleFederationTrustRequest(w http.ResponseWriter, r *http.Request) {
	dismiss := strings.HasSuffix(r.URL.Path, "/dismiss")
	if !requireHuman(w, r, "send or dismiss a federation trust request") {
		return
	}
	var req fedTrustRequestReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	req.Instance = strings.TrimSpace(req.Instance)
	rt := currentFederation()
	if rt == nil {
		writeError(w, http.StatusConflict, "federation_off", "federation is not running")
		return
	}
	if dismiss {
		rt.trustReqMu.Lock()
		if _, ok := rt.trustReq.in[req.Instance]; ok {
			delete(rt.trustReq.in, req.Instance)
			if rt.trustReq.dismissed == nil {
				rt.trustReq.dismissed = map[string]time.Time{}
			}
			rt.trustReq.dismissed[req.Instance] = time.Now()
			if len(rt.trustReq.dismissed) > fedTrustRequestMax {
				rt.trustReq.pruneIn(time.Now())
			}
		}
		rt.trustReqMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instance_id": req.Instance})
		return
	}
	note, fits := cleanTrustNote(req.Note)
	if !fits {
		writeError(w, http.StatusBadRequest, "invalid_arg", "the note is longer than 200 characters")
		return
	}
	if req.Instance == rt.id.ID() {
		writeError(w, http.StatusBadRequest, "invalid_arg", "this is this node")
		return
	}
	key := rt.directoryKey(req.Instance)
	if key == nil {
		writeError(w, http.StatusNotFound, "not_found", "no instance "+req.Instance+" on the hub")
		return
	}
	trusted, err := db.GetFederationPeer(req.Instance)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	now := time.Now()
	rt.trustReqMu.Lock()
	if rt.trustReq.out == nil {
		rt.trustReq.out = map[string]time.Time{}
	}
	last := rt.trustReq.out[req.Instance]
	if now.Sub(last) < fedTrustRequestCooldown {
		rt.trustReqMu.Unlock()
		writeError(w, http.StatusTooManyRequests, "rate_limited", "already asked this instance a moment ago")
		return
	}
	rt.trustReq.out[req.Instance] = now
	rt.trustReqMu.Unlock()
	if err := rt.sendEnrollment(key, proto.KindTrustRequest, "", proto.TrustRequest{Note: note, TrustsYou: trusted != nil}); err != nil {
		rt.trustReqMu.Lock()
		if rt.trustReq.out[req.Instance].Equal(now) {
			if last.IsZero() {
				delete(rt.trustReq.out, req.Instance)
			} else {
				rt.trustReq.out[req.Instance] = last
			}
		}
		rt.trustReqMu.Unlock()
		writeError(w, http.StatusBadGateway, "send_failed", "could not deliver the trust request: "+err.Error())
		return
	}
	setAuditTargetLabel(r, req.Instance)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instance_id": req.Instance, "trusts_you": trusted != nil})
}
