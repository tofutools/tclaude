package agentd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// config_remote.go lets a trusted peer administer this node's Config tab
// through the peer view: the dashboard reaches it as
// /api/peer/{node}/api/config, the same path and wire shape as the local tab,
// so the tab's code is shared.
//
//   - GET: the config as canonical JSON with every secret value withheld
//     (config_classes.go), its version, and what the caller may change.
//   - POST (?dry_run=1): the tab's full-config save, {config, base, clear}.
//   - PATCH: a partial update, {version, set: {path: value}, clear: [path]}.
//
// Every write is checked on this node, per changed key, against the grant of
// the key's class; a refused key refuses the whole change. Secret values never
// leave the node, not even for an unrestricted peer: one sent back empty is
// kept, one sent non-empty replaces it (and counts as a change, so a peer
// cannot confirm a guessed value), and clear removes one.

// configCaller is who reads or changes the config.
type configCaller struct {
	actor        string
	peer         string
	local        bool
	unrestricted bool
}

func peerConfigCaller(v *peerView) configCaller {
	return configCaller{actor: "operator@" + v.peer.InstanceID, peer: v.peer.InstanceID, unrestricted: v.unrestricted()}
}

func localConfigCaller() configCaller { return configCaller{actor: "operator", local: true} }

func (c configCaller) mayRead() bool {
	return c.local || c.unrestricted || fedPeerHasUnscopedGrant(c.peer, PermNodeConfigRead)
}

func (c configCaller) mayWrite(class configClass) bool {
	if c.local || c.unrestricted {
		return true
	}
	slug := configClassSlug[class]
	return slug != "" && c.mayRead() && fedPeerHasUnscopedGrant(c.peer, slug)
}

func (c configCaller) level() string {
	switch {
	case c.local:
		return "local"
	case c.unrestricted:
		return db.FederationTrustUnrestricted
	}
	return db.FederationTrustRestricted
}

// configAccess is what the caller may change, per class, with the key table
// the UI maps its fields onto (each form field names its dotted path).
type configAccess struct {
	Level   string                 `json:"level"`
	Classes map[configClass]string `json:"classes"`
	Slugs   map[configClass]string `json:"slugs"`
	Keys    []configKeyInfo        `json:"keys"`
}

func (c configCaller) access() configAccess {
	out := configAccess{Level: c.level(), Classes: map[configClass]string{}, Slugs: map[configClass]string{}, Keys: configKeyTable()}
	for _, class := range configClasses {
		out.Classes[class] = "read"
		if c.mayWrite(class) {
			out.Classes[class] = "write"
		}
		out.Slugs[class] = configClassSlug[class]
	}
	return out
}

// configRedaction names a withheld secret and whether it is set.
type configRedaction struct {
	Path string `json:"path"`
	Set  bool   `json:"set"`
}

func configToTree(cfg *config.Config) (map[string]any, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return decodeConfigTree(raw)
}

func decodeConfigTree(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree map[string]any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	if tree == nil {
		tree = map[string]any{}
	}
	return tree, nil
}

func treeToConfig(tree map[string]any) (*config.Config, error) {
	raw, err := json.Marshal(tree)
	if err != nil {
		return nil, err
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// configViewRaw is the canonical config as the caller may see it.
func configViewRaw(cfg *config.Config, c configCaller) (string, []configRedaction, error) {
	if c.local {
		raw, err := json.MarshalIndent(cfg, "", "  ")
		return string(raw), nil, err
	}
	tree, err := configToTree(cfg)
	if err != nil {
		return "", nil, err
	}
	redacted := []configRedaction{}
	for _, site := range configSecretSites(tree) {
		if _, present := site.parent[site.key]; !present {
			continue
		}
		redacted = append(redacted, configRedaction{Path: site.path, Set: secretValueSet(site.value())})
		delete(site.parent, site.key)
	}
	slices.SortFunc(redacted, func(a, b configRedaction) int { return strings.Compare(a.Path, b.Path) })
	shown, err := treeToConfig(tree)
	if err != nil {
		return "", nil, err
	}
	raw, err := json.MarshalIndent(shown, "", "  ")
	return string(raw), redacted, err
}

func configVersion(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

// configView is the GET answer, and what a save returns.
func configView(cfg *config.Config, loadErr error, c configCaller) (map[string]any, error) {
	raw, redacted, err := configViewRaw(cfg, c)
	if err != nil {
		return nil, err
	}
	resp := map[string]any{
		"raw":     raw,
		"version": configVersion(raw),
		"access":  c.access(),
	}
	if !c.local {
		resp["redacted"] = redacted
	}
	// The path names this node's home directory.
	if c.local || c.unrestricted {
		resp["path"] = config.ConfigPath()
	}
	if loadErr != nil {
		resp["malformed"] = true
		resp["warning"] = "the config file could not be parsed (" + loadErr.Error() +
			") — the form shows defaults, NOT your current settings; saving will replace the corrupt file entirely"
		if !c.local {
			resp["warning"] = "the node's config file could not be parsed — the form shows defaults; it can only be repaired on the node itself"
		}
	}
	if unknown := unknownConfigKeys(); len(unknown) > 0 {
		resp["unknown_keys"] = unknown
	}
	addExternalStewardInfo(resp, cfg)
	return resp, nil
}

func servePeerConfig(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	c := peerConfigCaller(v)
	if !c.mayRead() {
		writeError(w, http.StatusForbidden, "permission_denied", PermNodeConfigRead+" is not shared")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		cfg, loadErr := config.Load()
		resp, err := configView(cfg, loadErr, c)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "config view failed")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodPost, http.MethodPatch:
		serveConfigChange(w, r, c)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET, POST or PATCH only")
	}
}

// configChangeBody is the POST and PATCH body. POST carries the whole edited
// config and the base it was edited from; PATCH carries set paths and the
// version it was based on.
type configChangeBody struct {
	Config  json.RawMessage            `json:"config,omitempty"`
	Base    string                     `json:"base,omitempty"`
	Version string                     `json:"version,omitempty"`
	Set     map[string]json.RawMessage `json:"set,omitempty"`
	Clear   []string                   `json:"clear,omitempty"`
}

// configRefusal aborts a change with an HTTP answer.
type configRefusal struct {
	status int
	body   map[string]any
}

func (e *configRefusal) Error() string { return fmt.Sprint(e.body["error"]) }

func configRefuse(status int, code, msg string) *configRefusal {
	return &configRefusal{status, map[string]any{"code": code, "error": msg}}
}

// configDenied is one changed key the caller may not change.
type configDenied struct {
	Path     string      `json:"path"`
	Class    configClass `json:"class"`
	Requires string      `json:"requires"`
}

// serveConfigChange is the write path for a peer, and the local PATCH.
func serveConfigChange(w http.ResponseWriter, r *http.Request, c configCaller) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body configChangeBody
	if err := dec.Decode(&body); err != nil {
		configErrors(w, "request body is not valid JSON: "+err.Error())
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		configErrors(w, "only one JSON object is allowed")
		return
	}
	patch := r.Method == http.MethodPatch
	if patch && body.Config != nil || !patch && body.Set != nil {
		configErrors(w, `POST takes "config", PATCH takes "set"`)
		return
	}
	if !patch && body.Config == nil {
		configErrors(w, `request body is missing the "config" object`)
		return
	}
	dryRun := r.URL.Query().Get("dry_run") == "1"

	var previous config.Config
	var result *config.Config
	var changed, secretsChanged []string
	mutate := func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return configRefuse(http.StatusConflict, "malformed_target", "config.json on this node is corrupt; it can only be repaired on the node itself")
		}
		res, ch, sec, err := computeConfigChange(cfg, body, patch, c)
		if err != nil {
			return err
		}
		previous, result, changed, secretsChanged = *cfg, res, ch, sec
		if !dryRun {
			*cfg = *res
		}
		return nil
	}
	var err error
	if dryRun {
		cfg, loadErr := config.Load()
		err = mutate(cfg, loadErr)
	} else {
		_, err = config.Update(mutate)
	}
	var refusal *configRefusal
	if errors.As(err, &refusal) {
		if !dryRun {
			recordFederationAudit("federation.node.config", c.actor, "", "", "refused: "+refusal.Error(), refusal.status)
		}
		writeJSON(w, refusal.status, refusal.body)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_write_failed", "the config could not be saved")
		return
	}
	if dryRun {
		raw, _, err := configViewRaw(result, c)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "config view failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"raw": raw, "changed": changed, "secrets_changed": secretsChanged})
		return
	}
	recordFederationAudit("federation.node.config", c.actor, "", "", "changed: "+strings.Join(changed, ","), http.StatusOK)
	_ = session.SyncClaudeCleanupPeriod(result)
	tellStewardsNewName(&previous, result)
	if len(stewardConvs()) > 0 {
		autoFetchKnowledgeSource(result)
	}
	resp, err := configView(result, nil, c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "config view failed")
		return
	}
	resp["changed"] = changed
	writeJSON(w, http.StatusOK, resp)
}

// computeConfigChange builds the config a change leads to, from current and
// only the keys the change touched, after checking each of them against the
// caller's grants. It returns the changed leaf paths and the secrets the
// change sets or clears.
func computeConfigChange(current *config.Config, body configChangeBody, patch bool, c configCaller) (*config.Config, []string, []string, error) {
	viewRaw, _, err := configViewRaw(current, c)
	if err != nil {
		return nil, nil, nil, err
	}
	// Drift guard: a change made against an older view is refused, and a
	// peer must name the view it changed.
	switch {
	case body.Base != "" && body.Base != viewRaw, body.Version != "" && body.Version != configVersion(viewRaw):
		return nil, nil, nil, configRefuse(http.StatusConflict, "config_drift", "the config changed since it was loaded — reload it and re-apply your edits")
	case !c.local && body.Base == "" && body.Version == "":
		return nil, nil, nil, configRefuse(http.StatusBadRequest, "invalid_arg", `a change must name the config it was made against: "base" or "version"`)
	}

	curTree, err := configToTree(current)
	if err != nil {
		return nil, nil, nil, err
	}
	submitted, err := submittedConfigTree(body, patch, viewRaw)
	if err != nil {
		return nil, nil, nil, err
	}

	// Secrets: withheld values come back empty and are kept; a sent value
	// replaces; clear removes. Each one sent or cleared is a change of its
	// key, whatever the value.
	touched := map[string]bool{}
	var secretsChanged []string
	if !c.local {
		current := map[string]any{}
		for _, site := range configSecretSites(curTree) {
			current[site.path] = site.value()
		}
		clear := map[string]bool{}
		for _, p := range body.Clear {
			if !secretValueSet(current[p]) {
				return nil, nil, nil, configRefuse(http.StatusBadRequest, "invalid_arg", "clear names "+p+", which is not a secret that is set")
			}
			clear[p] = true
		}
		for _, site := range configSecretSites(submitted) {
			switch {
			case clear[site.path]:
				delete(site.parent, site.key)
				delete(clear, site.path)
			case secretValueSet(site.value()):
			case secretValueSet(current[site.path]):
				site.parent[site.key] = current[site.path]
				continue
			default:
				continue
			}
			touched[site.leaf] = true
			secretsChanged = append(secretsChanged, site.path)
		}
		for p := range clear {
			return nil, nil, nil, configRefuse(http.StatusBadRequest, "invalid_arg", "clear names "+p+", which the change removes")
		}
	} else if len(body.Clear) > 0 {
		return nil, nil, nil, configRefuse(http.StatusBadRequest, "invalid_arg", "clear is for a peer; the node's operator sets the value")
	}

	var changed []string
	var denied []configDenied
	for _, leaf := range configLeafPaths() {
		before, hadBefore := treeLookup(curTree, leaf)
		after, hasAfter := treeLookup(submitted, leaf)
		if !touched[leaf] && hadBefore == hasAfter && configValueEqual(before, after) {
			continue
		}
		changed = append(changed, leaf)
		class, _ := configKeyClass(leaf)
		if !c.mayWrite(class) {
			requires := configClassSlug[class]
			if requires == "" {
				requires = "unrestricted trust"
			}
			denied = append(denied, configDenied{Path: leaf, Class: class, Requires: requires})
		}
		if hasAfter {
			treeSet(curTree, leaf, after)
		} else {
			treeDelete(curTree, leaf)
		}
	}
	if len(denied) > 0 {
		paths := make([]string, len(denied))
		for i, d := range denied {
			paths[i] = d.Path + " (" + d.Requires + ")"
		}
		ref := configRefuse(http.StatusForbidden, "permission_denied", "not yours to change here: "+strings.Join(paths, ", "))
		ref.body["denied"] = denied
		return nil, nil, nil, ref
	}

	result, err := treeToConfig(curTree)
	if err != nil {
		return nil, nil, nil, configRefuse(http.StatusBadRequest, "invalid_arg", "the change is not a valid config: "+err.Error())
	}
	if errs := config.Validate(result); len(errs) > 0 {
		return nil, nil, nil, &configRefusal{http.StatusBadRequest, map[string]any{"code": "invalid_arg", "error": errs[0], "errors": errs}}
	}
	if name, named := config.AskSandboxProfileName(result.AskSandboxSetting()); named {
		if _, err := db.ResolveNamedSandboxSnapshot(name); err != nil {
			return nil, nil, nil, configRefuse(http.StatusBadRequest, "invalid_arg", fmt.Sprintf("ask.sandbox profile %q is unavailable; choose an existing library profile, auto, global, or off", name))
		}
	}
	config.Normalize(result)
	if c.local || slices.ContainsFunc(changed, func(p string) bool { return strings.HasPrefix(p, "notifications.") }) {
		result.Notifications.MarkNotifyTypesKnown()
	}
	return result, changed, secretsChanged, nil
}

// submittedConfigTree is the config the change asks for: a POST's typed
// config, or a PATCH's set paths applied to the caller's view.
func submittedConfigTree(body configChangeBody, patch bool, viewRaw string) (map[string]any, error) {
	if !patch {
		var typed config.Config
		if err := json.Unmarshal(body.Config, &typed); err != nil {
			return nil, configRefuse(http.StatusBadRequest, "invalid_arg", "config is not valid: "+err.Error())
		}
		return configToTree(&typed)
	}
	tree, err := decodeConfigTree([]byte(viewRaw))
	if err != nil {
		return nil, err
	}
	leaves := configLeafPaths()
	for path, raw := range body.Set {
		if _, ok := slices.BinarySearch(leaves, path); !ok {
			return nil, configRefuse(http.StatusBadRequest, "invalid_arg", "set names "+path+", which is not a config key")
		}
		value, err := decodeConfigValue(raw)
		if err != nil {
			return nil, configRefuse(http.StatusBadRequest, "invalid_arg", "set "+path+": "+err.Error())
		}
		treeSet(tree, path, value)
	}
	// Round-trip through the typed config, as a POST does: a value of the
	// wrong type is refused here, and the tree takes the canonical shape.
	typed, err := treeToConfig(tree)
	if err != nil {
		return nil, configRefuse(http.StatusBadRequest, "invalid_arg", "the change is not a valid config: "+err.Error())
	}
	return configToTree(typed)
}

func decodeConfigValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

func treeLookup(tree map[string]any, path string) (any, bool) {
	var cur any = tree
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func treeSet(tree map[string]any, path string, v any) {
	segs := strings.Split(path, ".")
	cur := tree
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = v
}

func treeDelete(tree map[string]any, path string) {
	segs := strings.Split(path, ".")
	cur := tree
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
	delete(cur, segs[len(segs)-1])
}

func configValueEqual(a, b any) bool {
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ra, rb)
}
