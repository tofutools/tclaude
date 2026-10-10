package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const PermModelsProxy = "models.proxy"

func validModelProxyName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if c < 'a' || c > 'z' {
			if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' && c != '.' {
				return false
			}
			continue
		}
	}
	return name != "." && name != ".."
}
func validModelProxyScope(scope string) bool {
	return strings.HasPrefix(scope, "http_proxy=") && validModelProxyName(strings.TrimPrefix(scope, "http_proxy="))
}
func fedPeerModelAllows(peer, name string) bool {
	instance, err := modelProxyPolicy(name)
	if err != nil {
		return false
	}
	for _, blocked := range instance.ModelPolicy.BlockedPeers {
		successor, resolveErr := db.ResolveFederationIdentitySuccessor(blocked)
		if resolveErr != nil || blocked == peer || successor == peer {
			return false
		}
	}
	p, err := db.GetFederationPeer(peer)
	if err != nil || p == nil {
		return false
	}
	if db.FederationPeerUnrestricted(peer) {
		return true
	}
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil {
		return false
	}
	for _, g := range grants {
		if g.Slug == PermModelsProxy && (g.Scope == "" || g.Scope == "http_proxy="+name) {
			return true
		}
	}
	return false
}
func modelProxyPolicy(name string) (config.HTTPProxyConfig, error) {
	cfg, err := config.Load()
	if err != nil || cfg.Agent == nil || cfg.Agent.ModelProxyDisabled {
		return config.HTTPProxyConfig{}, errors.New("model gateway is disabled by the operator")
	}
	instance, ok := cfg.Agent.HTTPProxies[name]
	p := instance.ModelPolicy
	if !ok || p == nil || !p.Enabled {
		return instance, errors.New("named model gateway is disabled or absent")
	}
	if (p.Dialect != "" && p.Dialect != "anthropic" && p.Dialect != "openai") || (p.Dialect == "openai" && p.PrecountInput) {
		return instance, errors.New("model gateway dialect must be anthropic or openai; OpenAI token precounting is not supported")
	}
	if len(p.Models) == 0 || p.DailyRequests <= 0 || p.DailyTokens <= 0 || p.PeerDailyRequests <= 0 || p.PeerDailyTokens <= 0 || p.SessionDailyRequests <= 0 || p.SessionDailyTokens <= 0 || p.MaxInputTokens <= 0 || p.MaxInputTokens > 100000000 || p.MaxOutputTokens <= 0 || p.MaxOutputTokens > 10000000 || p.MaxConcurrent < 1 || p.MaxConcurrent > 128 || p.RequestsPerMinute < 1 || p.MaxRequestBytes < 0 || p.MaxRequestBytes > 16<<20 || p.MaxResponseBytes < 0 || p.MaxResponseBytes > 256<<20 || p.MaxEventBytes < 0 || p.MaxEventBytes > 4<<20 || p.MaxDurationSeconds < 0 || p.MaxDurationSeconds > 3600 {
		return instance, errors.New("operator must configure model allowlist, positive daily budgets, token bounds and concurrency/rate limits")
	}
	if _, err = httpProxyURL(instance.URL, ""); err != nil || !httpProxyHeaderAllowed(instance.Header) {
		return instance, errors.New("operator must fix the named model gateway URL or credential header")
	}
	return instance, nil
}

type modelDialectWriter struct {
	http.ResponseWriter
	openai bool
}

func (w modelDialectWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func modelWriterOpenAI(w http.ResponseWriter) bool {
	d, ok := w.(modelDialectWriter)
	return ok && d.openai
}
func modelError(w http.ResponseWriter, status int, message string) {
	kind := "invalid_request_error"
	switch status {
	case 401:
		kind = "authentication_error"
	case 403:
		kind = "permission_error"
	case 429:
		kind = "rate_limit_error"
	case 502, 503, 504:
		kind = "api_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}}
	if d, ok := w.(modelDialectWriter); ok && d.openai {
		body = map[string]any{"error": map[string]any{"type": kind, "message": message, "param": nil, "code": nil}}
	}
	_ = json.NewEncoder(w).Encode(body)
}
func modelLaunchAllowed(r *http.Request, row *db.SessionRow, peer, name string) bool {
	if row == nil {
		return false
	}
	if row.ConvID != "" {
		actor, err := db.GetAgentByConv(row.ConvID)
		if err != nil || actor != nil && (!actor.Active() || actor.CurrentConvID != row.ConvID) {
			return false
		}
		allowed, _, err := permissionAllowsAction(r, row.ConvID, PermModelsProxy, ActionContext{RemotePeer: peer, HTTPProxy: name})
		return err == nil && allowed
	}
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	src := permSources{resolvable: true, group: map[string][]string{}}
	if value, ok := httpProxyLaunchGroups.Load(row.ID); ok {
		launch := value.(httpProxyLaunchGroup)
		if time.Now().Before(launch.expires) {
			src.override = launch.overrides
			grants, err := db.ListAgentGroupPermissionRows(launch.groupID)
			if err != nil {
				return false
			}
			for _, g := range grants {
				src.group[g.Slug] = append(src.group[g.Slug], g.ScopeJSON)
			}
		}
	}
	verdict := resolvePermissionVerdictFrom(src, PermModelsProxy, cfg.HasDefaultPermission(PermModelsProxy))
	return verdict.Resolution == permAllow && evalPermissionScope(verdict, "", ActionContext{RemotePeer: peer, HTTPProxy: name}).Satisfied
}
func resolveModelProxyReference(ref string) (*db.FederationPeer, string, error) {
	name, selector, ok := strings.Cut(ref, "@")
	if !ok || !validModelProxyName(name) || selector == "" || strings.Contains(selector, "@") {
		return nil, "", errors.New("model proxy must be <name>@<trusted peer>")
	}
	peer, err := resolveFederationPeer(selector)
	return peer, name, err
}
func handleModelProxyBind(w http.ResponseWriter, r *http.Request) {
	row, _ := r.Context().Value(httpProxyLaunchRowKey{}).(*db.SessionRow)
	if row == nil {
		modelError(w, 403, "model gateway registration requires a verified live launch pane")
		return
	}
	var in struct {
		Harness   string `json:"harness"`
		Reference string `json:"reference"`
		Hash      string `json:"bearer_hash"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
		modelError(w, 400, "invalid model gateway registration")
		return
	}
	peer, name, err := resolveModelProxyReference(in.Reference)
	if err != nil || !modelLaunchAllowed(r, row, peerID(peer), name) {
		modelError(w, 403, "model gateway requires models.proxy covering the selected peer and named proxy")
		return
	}
	launchHarness := row.Harness
	if launchHarness == "" {
		launchHarness = harness.DefaultName
	}
	h, _ := harness.Get(launchHarness)
	if h == nil || !h.SupportsModelProxy() || (in.Harness != "" && in.Harness != h.Name) {
		modelError(w, 400, "model gateway harness protocol does not match the live launch")
		return
	}
	// Pin the instance ID rather than a mutable operator alias.
	reference := name + "@" + peer.InstanceID
	var lease *db.ModelProxyWorkerLease
	if worker := modelLeaseWorker(row); worker != "" {
		lease, err = db.GetModelProxyWorkerLease(worker)
		if err != nil {
			modelError(w, 503, "requester gateway lease state unavailable")
			return
		}
	}
	leaseID := ""
	rt := currentFederation()
	if lease != nil {
		if lease.Gateway != peer.InstanceID || lease.Proxy != name || rt == nil {
			modelError(w, 403, "requester gateway launch does not match its issued lease")
			return
		}
		leaseID = lease.Lease
	}
	if err = db.BindModelProxyLaunchPendingLease(row.ID, reference, in.Hash, leaseID); err != nil {
		modelError(w, 403, "model gateway registration refused: generation already bound, revoked or absent")
		return
	}
	if lease != nil {
		if err = rt.activateModelLease(r.Context(), peer, *lease, row); err != nil {
			modelError(w, 503, err.Error())
			return
		}
	}
	if h.ModelProxyDialect() == "openai" {
		if rt == nil {
			modelError(w, 503, "model gateway federation disconnected")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := rt.checkModelDialect(ctx, peer, row.ID, name, "openai", &db.ModelProxyLease{ID: leaseID, Generation: row.ExitLaunchGeneration}); err != nil {
			_ = db.RevokeModelProxyLaunch(row.ID, row.ExitLaunchGeneration)
			modelError(w, 503, h.DisplayName+" model gateway requires an available OpenAI Responses dialect")
			return
		}
	}
	if lease != nil {
		if err = db.SetModelProxyLaunchLease(row.ID, row.ExitLaunchGeneration, leaseID); err != nil {
			modelError(w, 403, "requester gateway lease binding refused")
			return
		}
		recordFederationAudit("models.lease.worker", peer.InstanceID, lease.Worker, name, "request="+lease.Request+" lease="+leaseID+" payer="+peer.InstanceID+" generation="+row.ExitLaunchGeneration, 200)
	}
	writeJSON(w, 200, map[string]any{"reference": reference})
}
func peerID(p *db.FederationPeer) string {
	if p == nil {
		return ""
	}
	return p.InstanceID
}
func modelBoundCaller(r *http.Request) (*db.SessionRow, *db.ModelProxyLaunch, *db.FederationPeer, string, error) {
	row, _ := r.Context().Value(httpProxyLaunchRowKey{}).(*db.SessionRow)
	if row == nil {
		return nil, nil, nil, "", db.ErrModelProxyRefused
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, nil, nil, "", db.ErrModelProxyRefused
	}
	launch, err := db.VerifyModelProxyLaunch(row.ID, bearer)
	if err != nil {
		return nil, nil, nil, "", err
	}
	peer, name, err := resolveModelProxyReference(launch.Reference)
	if err != nil || !modelLaunchAllowed(r, row, peerID(peer), name) {
		return nil, nil, nil, "", db.ErrModelProxyRefused
	}
	return row, launch, peer, name, nil
}
func handleModelProxyRevoke(w http.ResponseWriter, r *http.Request) {
	row, launch, _, _, err := modelBoundCaller(r)
	if err != nil {
		modelError(w, 403, "model gateway launch is no longer authorized")
		return
	}
	if db.RevokeModelProxyLaunch(row.ID, launch.Generation) != nil {
		modelError(w, 503, "model gateway revocation unavailable")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func handleModelProxyRequest(w http.ResponseWriter, r *http.Request) {
	w = modelDialectWriter{w, strings.HasSuffix(r.URL.Path, "/responses")}
	row, launch, peer, name, err := modelBoundCaller(r)
	if err != nil {
		modelError(w, 403, "model gateway launch is absent, revoked, replaced or lacks models.proxy")
		return
	}
	rt := currentFederation()
	if rt == nil {
		modelError(w, 503, "model gateway federation is disconnected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	dialect := "anthropic"
	if h, _ := harness.Get(row.Harness); h != nil && h.ModelProxyDialect() == "openai" {
		dialect = "openai"
	}
	path := r.PathValue("path")
	if path == "" {
		path = strings.TrimPrefix(r.URL.Path, "/v1/models/request/")
	}
	if path != "v1/models" && ((dialect == "openai") != (path == "v1/responses")) {
		modelError(w, 400, "endpoint does not match this launch's harness protocol")
		return
	}
	conn, err := rt.openModelStream(ctx, peer, launch.Session, name, dialect, &db.ModelProxyLease{ID: launch.Lease, Generation: launch.Generation})
	if err != nil {
		modelError(w, 503, "model gateway unavailable or refused by peer")
		return
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	// Revocation, retirement and generation replacement also interrupt a quiet
	// stream. This reads authorization only; it never gathers local status.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, e := db.VerifyModelProxyLaunch(row.ID, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
				fresh, e2 := db.LoadSession(row.ID)
				if e != nil || e2 != nil || fresh == nil || current.Generation != launch.Generation || !modelLaunchAllowed(r, fresh, peer.InstanceID, name) {
					cancel()
					return
				}
			}
		}
	}()
	relayModelRequest(w, r, conn)
}
func handleModelProxyUsage(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "inspect model gateway usage") {
		return
	}
	day := r.URL.Query().Get("day")
	if day == "" {
		day = time.Now().UTC().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		modelError(w, 400, "day must be YYYY-MM-DD in UTC")
		return
	}
	usage, err := db.ListModelProxyUsage(day)
	if err != nil {
		modelError(w, 503, "model gateway usage unavailable")
		return
	}
	writeJSON(w, 200, usage)
}

// Control is operator-only and exposes no provider URL, headers or credentials.
func handleModelProxyControl(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "control model gateways") {
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		cfg, err := config.Load()
		if err != nil {
			modelError(w, 503, "model gateway configuration unavailable")
			return
		}
		policies := map[string]*config.ModelProxyPolicy{}
		disabled := false
		if cfg.Agent != nil {
			disabled = cfg.Agent.ModelProxyDisabled
			for name, instance := range cfg.Agent.HTTPProxies {
				if instance.ModelPolicy != nil {
					policies[name] = instance.ModelPolicy
				}
			}
		}
		writeJSON(w, 200, map[string]any{"disabled": disabled, "gateways": policies})
		return
	}
	var in struct {
		Name     string `json:"name"`
		Peer     string `json:"peer"`
		Disabled bool   `json:"disabled"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
		modelError(w, 400, "invalid model gateway switch")
		return
	}
	if in.Peer != "" {
		peer, err := resolveFederationPeer(in.Peer)
		if err != nil {
			modelError(w, 400, "select a trusted peer")
			return
		}
		in.Peer = peer.InstanceID
		if in.Name == "" {
			modelError(w, 400, "a peer switch requires a named gateway")
			return
		}
	}
	if in.Disabled {
		if err := db.RevokeModelProxyLeaseSelection(in.Name, in.Peer); err != nil {
			modelError(w, 503, "model gateway lease revocation unavailable")
			return
		}
	}
	_, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		if cfg.Agent == nil {
			return errors.New("agent configuration is absent")
		}
		if in.Name == "" {
			cfg.Agent.ModelProxyDisabled = in.Disabled
			return nil
		}
		instance, ok := cfg.Agent.HTTPProxies[in.Name]
		if !ok || instance.ModelPolicy == nil {
			return errors.New("named model gateway is absent")
		}
		if in.Peer == "" {
			instance.ModelPolicy.Enabled = !in.Disabled
		} else {
			peers := []string{}
			for _, peer := range instance.ModelPolicy.BlockedPeers {
				if peer != in.Peer {
					peers = append(peers, peer)
				}
			}
			if in.Disabled {
				peers = append(peers, in.Peer)
			}
			instance.ModelPolicy.BlockedPeers = peers
		}
		cfg.Agent.HTTPProxies[in.Name] = instance
		return nil
	})
	if err != nil {
		modelError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"name": in.Name, "peer": in.Peer, "disabled": in.Disabled})
}

func validateLaunchModelProxy(h *harness.Harness) func(string) (string, error) {
	return func(ref string) (string, error) {
		if ref == "off" {
			return ref, nil
		}
		if !h.SupportsModelProxy() {
			return "", errors.New(h.ModelProxyRefusal())
		}
		peer, name, err := resolveModelProxyReference(ref)
		if err != nil {
			return "", err
		}
		return name + "@" + peer.InstanceID, nil
	}
}
