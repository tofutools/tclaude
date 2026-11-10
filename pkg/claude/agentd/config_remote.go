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
	switch {
	case c.local:
		return true
	case class == configClassLocal:
		// The node's own, whatever the peer's trust.
		return false
	case c.unrestricted:
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
		label := "refused: "
		if dryRun {
			label = "refused (dry run): "
		}
		if !c.local {
			recordFederationAudit("federation.node.config", c.actor, "", "", label+refusal.Error(), refusal.status)
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
	if !c.local {
		recordFederationAudit("federation.node.config", c.actor, "", "", "changed: "+strings.Join(changed, ","), http.StatusOK)
	}
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
	kept := map[string]bool{}
	var secretsChanged []string
	if !c.local {
		curSites, subSites := configSecretSites(curTree), configSecretSites(submitted)
		for _, site := range append(slices.Clone(curSites), subSites...) {
			if site.ambiguous {
				return nil, nil, nil, configRefuse(http.StatusBadRequest, "ambiguous_identity", site.path+" is empty or repeated, so a withheld secret cannot be matched to it; give each element its own name (on the node itself if the node's config holds it)")
			}
		}
		current := map[string]any{}
		for _, site := range curSites {
			current[site.path] = site.value()
		}
		clear := map[string]bool{}
		for _, p := range body.Clear {
			if !secretValueSet(current[p]) {
				return nil, nil, nil, configRefuse(http.StatusBadRequest, "invalid_arg", "clear names "+p+", which is not a secret that is set")
			}
			clear[p] = true
		}
		submittedPaths := map[string]bool{}
		for _, site := range subSites {
			submittedPaths[site.path] = true
			switch {
			case clear[site.path]:
				delete(site.parent, site.key)
				delete(clear, site.path)
			case secretValueSet(site.value()):
			case secretValueSet(current[site.path]):
				site.parent[site.key] = current[site.path]
				kept[site.path] = true
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
		// A renamed element would lose its withheld secret silently: an
		// element with a secret that goes, beside a new one without, is
		// refused until the secret is sent again (or the old one cleared).
		for _, gone := range curSites {
			if submittedPaths[gone.path] || !secretValueSet(gone.value()) {
				continue
			}
			for _, site := range subSites {
				if site.leaf == gone.leaf && current[site.path] == nil && !secretValueSet(site.value()) {
					return nil, nil, nil, configRefuse(http.StatusBadRequest, "secret_not_sent", gone.path+" is withheld and its element is renamed or replaced by "+site.path+": send the secret for "+site.path+" again")
				}
			}
		}
	} else if len(body.Clear) > 0 {
		return nil, nil, nil, configRefuse(http.StatusBadRequest, "invalid_arg", "clear is for a peer; the node's operator sets the value")
	}

	var changed []string
	var denied []configDenied
	if !c.local {
		// Before the merge below, which rewrites curTree.
		denied = configCredentialRefusals(curTree, submitted, kept)
	}
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
			switch {
			case class == configClassLocal:
				requires = "the node itself"
			case requires == "":
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
		ref := configRefuse(http.StatusForbidden, "permission_denied", "not yours to change here: "+deniedPaths(denied))
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
	if !c.local {
		// After Normalize, which renames legacy slugs to their current names.
		before, err := configToTree(current)
		if err != nil {
			return nil, nil, nil, err
		}
		after, err := configToTree(result)
		if err != nil {
			return nil, nil, nil, err
		}
		if denied := configDefaultPermissionRefusals(before, after); len(denied) > 0 {
			ref := configRefuse(http.StatusForbidden, "permission_denied", "a peer cannot add these to the permissions agents start with: "+deniedPaths(denied))
			ref.body["denied"] = denied
			return nil, nil, nil, ref
		}
	}
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

// configCredentialRefusals holds every peer, unrestricted included, to the
// credential bindings (configCredentialBindings): no credential file it set,
// and no new destination for a credential it did not send.
func configCredentialRefusals(curTree, submitted map[string]any, kept map[string]bool) []configDenied {
	var denied []configDenied
	deny := func(path, why string) {
		denied = append(denied, configDenied{Path: path, Class: configClassCredentials, Requires: why})
	}
	for _, b := range configCredentialBindings {
		parent, star := strings.CutSuffix(b.object, ".*")
		objects := map[string][2]map[string]any{}
		if star {
			cur, _ := treeLookup(curTree, parent)
			sub, _ := treeLookup(submitted, parent)
			curMap, _ := cur.(map[string]any)
			subMap, _ := sub.(map[string]any)
			for k, v := range subMap {
				o, _ := v.(map[string]any)
				c, _ := curMap[k].(map[string]any)
				objects[parent+"."+k] = [2]map[string]any{c, o}
			}
		} else {
			cur, _ := treeLookup(curTree, b.object)
			sub, _ := treeLookup(submitted, b.object)
			c, _ := cur.(map[string]any)
			o, _ := sub.(map[string]any)
			objects[b.object] = [2]map[string]any{c, o}
		}
		for path, pair := range objects {
			cur, sub := pair[0], pair[1]
			if sub == nil {
				continue
			}
			credentialKept := false
			for _, f := range b.files {
				v := sub[f]
				if !secretValueSet(v) {
					continue
				}
				if cur == nil || !configValueEqual(cur[f], v) {
					deny(path+"."+f, "the node itself (credential files are set there)")
					continue
				}
				credentialKept = true
			}
			if cur == nil {
				continue
			}
			for _, f := range b.secrets {
				credentialKept = credentialKept || kept[path+"."+f]
			}
			if !credentialKept {
				continue
			}
			for _, d := range b.dests {
				if !configValueEqual(cur[d], sub[d]) {
					deny(path+"."+d, "the credential sent again (its destination changes)")
				}
			}
		}
	}
	for leaf, field := range configCredentialFileLists {
		known := map[string]bool{}
		cur, _ := treeLookup(curTree, leaf)
		curList, _ := cur.([]any)
		for _, el := range curList {
			if m, ok := el.(map[string]any); ok {
				if f, _ := m[field].(string); f != "" {
					known[f] = true
				}
			}
		}
		sub, _ := treeLookup(submitted, leaf)
		subList, _ := sub.([]any)
		for i, el := range subList {
			if m, ok := el.(map[string]any); ok {
				if f, _ := m[field].(string); f != "" && !known[f] {
					deny(fmt.Sprintf("%s[%d].%s", leaf, i, field), "the node itself (credential files are set there)")
				}
			}
		}
	}
	slices.SortFunc(denied, func(a, b configDenied) int { return strings.Compare(a.Path, b.Path) })
	return denied
}

// configPermissionLeaves are the lists of slugs every new agent (or Steward)
// starts with.
var configPermissionLeaves = []string{"agent.default_permissions", "steward.default_permissions"}

// configDefaultPermissionRefusals keeps a peer from handing this node's
// agents what reaches past their own work: a risky slug, the human's
// channels, federation administration or the power to grant. The node's
// operator adds those; a peer may remove any.
func configDefaultPermissionRefusals(curTree, submitted map[string]any) []configDenied {
	var denied []configDenied
	for _, leaf := range configPermissionLeaves {
		cur, _ := treeLookup(curTree, leaf)
		sub, _ := treeLookup(submitted, leaf)
		had := map[string]bool{}
		curList, _ := cur.([]any)
		for _, v := range curList {
			if s, ok := v.(string); ok {
				had[s] = true
			}
		}
		subList, _ := sub.([]any)
		for _, v := range subList {
			slug, _ := v.(string)
			if had[slug] || !peerUnassignableSlug(slug) {
				continue
			}
			denied = append(denied, configDenied{Path: leaf + " (" + slug + ")", Class: configClassAgents, Requires: "the node itself"})
		}
	}
	return denied
}

func peerUnassignableSlug(slug string) bool {
	base, _, _ := strings.Cut(slug, ":")
	base = strings.TrimSpace(base)
	if slices.Contains(riskySlugs, base) {
		return true
	}
	for _, prefix := range []string{"human.", "federation.", "permissions.", "sudo", "*"} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

func deniedPaths(denied []configDenied) string {
	paths := make([]string, len(denied))
	for i, d := range denied {
		paths[i] = d.Path + " (" + d.Requires + ")"
	}
	return strings.Join(paths, ", ")
}
