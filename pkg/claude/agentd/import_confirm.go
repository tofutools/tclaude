package agentd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// import_confirm.go lets a human keep what a file import would otherwise drop.
// A file's elevated items are dropped by default (see fileGroupDrops and
// receiverLaunchFieldsProfile). Each drop that a human may reverse carries an
// importItem, and the preview lists them. The apply request names the items to
// keep and the digest of the file they were read from; this server checks both,
// and only a human on this node can keep anything.
//
// The sandbox is never an item, and neither is sudo: a file cannot widen the
// one, and the other is temporary.

// The headings an item is listed under, in the order a preview shows them.
const (
	importKindOwners      = "owners"
	importKindPermissions = "permissions"
	importKindPosture     = "posture"
	importKindLaunch      = "launch"
	importKindContext     = "context"
	importKindEnvironment = "environment"
	importKindCron        = "cron"
)

// importItem is one elevated thing a file holds that a human may keep.
type importItem struct {
	// ID is what an apply request names. It stays the same for the same file.
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	// Gives says what keeping it gives.
	Gives string `json:"gives"`
	// Default is whether it starts ticked. All do but what runs things or hides
	// a value: environment variables and scheduled jobs.
	Default bool `json:"default,omitempty"`
	// Revealable marks an environment variable, whose value only a separate
	// request returns. The value is never part of an item.
	Revealable bool `json:"revealable,omitempty"`
}

// importConfirmation is the preview's confirm list.
type importConfirmation struct {
	// Digest binds a confirmation to this file.
	Digest string `json:"digest"`
	// CanKeep says this caller may keep items. Anyone else sees the list and
	// keeps nothing.
	CanKeep bool         `json:"can_keep"`
	Items   []importItem `json:"items"`
	// Warnings are the preview's warnings without those the list already shows.
	Warnings []string `json:"warnings"`
}

// fileDrop is one thing taken out of a file. An item with no ID cannot be kept.
type fileDrop struct {
	item importItem
	// keep puts it back. A group file's closures ignore their argument.
	keep func(*spawnProfileJSON)

	// warning is the drop's own warning; the rest build one shared by a kind.
	warning string
	list    string // a dropList key, and name the entry in it
	name    string
	grant   *fileGrantLine // holds a single slug
}

// dropLists words a warning that names several drops of one kind.
var dropLists = map[string]func(names string) string{
	"environment": func(names string) string { return fileFieldDropped("environment variables " + names) },
	"context":     func(names string) string { return fileFieldDropped("context features " + names) },
	"owners": func(names string) string {
		return "This file names " + names + " as group owners, which this import does not keep, so they land as plain members."
	},
	"cron": func(names string) string {
		return "Scheduled jobs " + names + " land disabled. Review them and enable the ones you want."
	},
	"cron_unowned": func(names string) string {
		return "Scheduled jobs " + names + " no longer run as the operator. Set their owner before enabling them."
	},
}

// renderFileDrops words the warnings for drops, naming each kind once. Offered
// grants and those that cannot be offered read separately, so a preview can take
// the offered ones out of its warnings.
func renderFileDrops(drops []fileDrop) []string {
	var out []string
	lists := map[string]int{}
	names := map[string][]string{}
	var grants [2]fileGrants
	for _, d := range drops {
		switch {
		case d.warning != "":
			out = append(out, d.warning)
		case d.list != "":
			if _, seen := lists[d.list]; !seen {
				lists[d.list] = len(out)
				out = append(out, "")
			}
			names[d.list] = append(names[d.list], d.name)
		case d.grant != nil:
			i := 0
			if d.item.ID != "" {
				i = 1
			}
			grants[i].add(d.grant.kind, d.grant.holder, d.grant.slugs[0])
		}
	}
	for list, at := range lists {
		out[at] = dropLists[list](strings.Join(names[list], ", "))
	}
	for _, g := range grants {
		if len(g) > 0 {
			out = append(out, "This file grants "+strings.Join(g.lines(), "; ")+
				", which authority in a file does not carry, so none of it is applied. Grant what you want here yourself.")
		}
	}
	return out
}

// importKeep is what a request asked to keep, and whether it may.
type importKeep struct {
	// human is a human on this node. Nobody else keeps anything.
	human bool
	// named says the request made a selection. One that names nothing confirmed
	// nothing, whatever a preview would tick, so only owners land: a snapshot
	// spawn or a bare API call has shown nobody a list.
	named bool
	// defaults keeps what a preview ticks.
	defaults  bool
	all, none bool
	ids       map[string]bool
	digest    string
	// ignored is a selection from a caller who may not make one.
	ignored bool
}

// importForwardedKey marks a request a federation peer forwarded through its
// peer view (servePeerAsLocal). An unrestricted peer's request then passes the
// dashboard's checks as this node's operator would, but the person behind it is
// that peer's. Nothing a client sends can set it.
type importForwardedKey struct{}

// importKeepCaller reports a human on this node, the only caller who can keep a
// file's items. The dashboard stamps any cookie or remote-listener caller as the
// human, so a forwarded request is told apart here.
func importKeepCaller(r *http.Request) bool {
	if forwarded, _ := r.Context().Value(importForwardedKey{}).(bool); forwarded {
		return false
	}
	return classify(peerFromContext(r.Context())) == classHuman &&
		peerActionFromRequest(r) == nil && peerGrantFromRequest(r) == nil
}

// readImportKeep reads the selection off an import request.
func readImportKeep(r *http.Request) importKeep {
	return parseImportKeep(r.URL.Query(), importKeepCaller(r))
}

func parseImportKeep(q url.Values, human bool) importKeep {
	k := importKeep{
		human: human, all: q.Get("keep_all") == "true", none: q.Get("keep_none") == "true",
		defaults: q.Get("keep_defaults") == "true",
		ids:      map[string]bool{}, digest: strings.TrimSpace(q.Get("keep_digest")),
	}
	for _, id := range q["keep"] {
		k.ids[id] = true
	}
	k.named = k.all || k.none || k.defaults || len(k.ids) > 0
	k.ignored = k.named && !human
	return k
}

// preview is the selection a preview shows: the defaults, for this caller.
func (k importKeep) preview() importKeep { return importKeep{human: k.human} }

// keeps says whether this selection keeps an item that can be kept.
func (k importKeep) keeps(item importItem) bool {
	switch {
	case !k.human || item.ID == "":
		return false
	case !k.named:
		return item.Kind == importKindOwners
	}
	return !k.none && (k.all || k.ids[item.ID] || (k.defaults && item.Default))
}

// importKeepError refuses a selection, rather than drop what a human asked for.
type importKeepError struct {
	status int
	code   string
	msg    string
}

func (e *importKeepError) Error() string { return e.msg }

// check holds a selection to the file it arrived with: the digest of the file
// the human saw, and items that file really holds. Dropping needs no proof, and
// a caller who may not keep is told so in a warning rather than refused.
func (k importKeep) check(digest string, items []importItem) error {
	if !k.human || !k.named {
		return nil
	}
	if k.none && (k.all || k.defaults || len(k.ids) > 0) {
		return &importKeepError{http.StatusBadRequest, "invalid_arg", "keep_none cannot be combined with another selection"}
	}
	if k.none {
		return nil
	}
	if k.digest != digest {
		return &importKeepError{http.StatusConflict, "keep_stale",
			"keeping items needs the digest of the file they were read from, and this file's is " + digest +
				". Preview this file, check its list, then pass that digest with --keep-digest."}
	}
	for _, id := range slices.Sorted(mapsKeys(k.ids)) {
		if !slices.ContainsFunc(items, func(item importItem) bool { return item.ID == id }) {
			return &importKeepError{http.StatusConflict, "keep_unknown",
				fmt.Sprintf("this import has no elevated item %q to keep. Preview it again: what is listed depends on the file and the landing group.", id)}
		}
	}
	return nil
}

func mapsKeys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// importKeepIgnored is the warning for a selection from a caller who cannot make
// one.
const importKeepIgnored = "Only a human on this node can keep what a file asks for, so this selection was ignored and nothing was kept."

// fileImportOutcome is what a selection did to a file's drops.
type fileImportOutcome struct {
	warnings []string
	kept     []string
	dropped  []string
}

// settleFileDrops applies a selection: it puts back what is kept, and words the
// warnings for the rest.
func settleFileDrops(drops []fileDrop, k importKeep, p *spawnProfileJSON) fileImportOutcome {
	var out fileImportOutcome
	var unkept []fileDrop
	for _, d := range drops {
		if k.keeps(d.item) {
			d.keep(p)
			out.kept = append(out.kept, d.item.Label)
			continue
		}
		unkept = append(unkept, d)
		if d.item.ID != "" {
			out.dropped = append(out.dropped, d.item.Label)
		}
	}
	out.warnings = renderFileDrops(unkept)
	if k.ignored {
		out.warnings = append(out.warnings, importKeepIgnored)
	}
	if len(out.kept) > 0 {
		slog.Info("import: a human kept elevated items from a file", "items", out.kept)
	}
	return out
}

// fileImportItems lists the drops a human may reverse, by heading.
func fileImportItems(drops []fileDrop) []importItem {
	order := []string{importKindOwners, importKindPermissions, importKindPosture, importKindLaunch,
		importKindContext, importKindEnvironment, importKindCron}
	items := []importItem{}
	for _, kind := range order {
		for _, d := range drops {
			if d.item.ID != "" && d.item.Kind == kind {
				items = append(items, d.item)
			}
		}
	}
	return items
}

// newImportConfirmation builds a preview's list. warnings are all the preview's;
// those the list already shows are left out of its own.
func newImportConfirmation(digest string, drops []fileDrop, human bool, warnings []string) *importConfirmation {
	var offered []fileDrop
	for _, d := range drops {
		if d.item.ID != "" {
			offered = append(offered, d)
		}
	}
	listed := renderFileDrops(offered)
	return &importConfirmation{
		Digest: digest, CanKeep: human, Items: fileImportItems(drops),
		Warnings: slices.DeleteFunc(slices.Clone(warnings), func(w string) bool { return slices.Contains(listed, w) }),
	}
}

func archiveDigest(archive []byte) string {
	sum := sha256.Sum256(archive)
	return hex.EncodeToString(sum[:])
}

// permissionGives is what a slug's item says it gives.
func permissionGives(slug string) string {
	for _, perm := range permissionRegistry {
		if perm.Slug == slug {
			return perm.Description
		}
	}
	return slug
}

// spawnGivesPermission reports a grant a spawn into group would hold anyway: the
// install's default permissions, and the group's own unscoped ones.
func spawnGivesPermission(group, slug string) bool {
	if cfg, err := config.Load(); err == nil && cfg.Agent != nil && slices.Contains(cfg.Agent.DefaultPermissions, slug) {
		return true
	}
	if group == "" {
		return false
	}
	g, err := db.GetAgentGroupByName(group)
	if err != nil || g == nil {
		return false
	}
	rows, err := db.ListAgentGroupPermissionRows(g.ID)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(rows, func(row db.AgentGroupPermission) bool { return row.Slug == slug && row.ScopeJSON == "" })
}

// permissionItem is the item of a grant in a file, or has no ID when the grant
// cannot land here: an unknown slug, or a scope with no meaning on this node.
func permissionItem(id, label, slug, scopeJSON string) importItem {
	if !IsKnownPermSlug(slug) {
		return importItem{}
	}
	if _, err := canonicalImportedPermissionScopeForSlug(slug, scopeJSON); err != nil {
		return importItem{}
	}
	return importItem{ID: id, Kind: importKindPermissions, Label: label, Gives: permissionGives(slug), Default: true}
}

// fileGroupDrops takes out of a group's file what would become a live grant, and
// its environment, which would reach every spawn into the new group. Nothing here
// is a move the receiver owns: only a file reaches this path. A deny only takes
// away, so it stays. Scheduled jobs stay for a human to review, with none enabled
// and none speaking as the operator: an owner this file named could otherwise
// enable one. It runs before the import validates permission scopes, so a row
// that does not land cannot refuse the import.
func fileGroupDrops(exp *groupexport.Export) []fileDrop {
	titles := map[string]string{}
	for _, c := range exp.Convs {
		titles[c.ConvID] = c.Title
	}
	holder := func(conv string) string { return cmp.Or(titles[conv], short8(conv)) }
	var drops []fileDrop

	owners := exp.Owners
	exp.Owners = nil
	for _, o := range owners {
		name := holder(o.ConvID)
		drops = append(drops, fileDrop{
			item: importItem{ID: "owner:" + o.ConvID, Kind: importKindOwners, Label: "owner " + name, Default: true,
				Gives: "Owns the group: it can manage the group's members, with every owner-implied permission."},
			keep: func(*spawnProfileJSON) { exp.Owners = append(exp.Owners, o) },
			list: "owners", name: name,
		})
	}

	environment := exp.Group.Environment
	exp.Group.Environment = nil
	for _, entry := range environment {
		drops = append(drops, fileDrop{
			item: importItem{ID: "env:" + entry.Name, Kind: importKindEnvironment, Label: "environment variable " + entry.Name,
				Gives: "Set in every agent spawned into this group. It can change what a harness loads or how it decides.", Revealable: true},
			keep: func(*spawnProfileJSON) { exp.Group.Environment = append(exp.Group.Environment, entry) },
			list: "environment", name: entry.Name,
		})
	}

	permissions := exp.Group.Permissions
	exp.Group.Permissions = nil
	for _, p := range permissions {
		slug := db.CanonicalPermissionSlug(p.Slug)
		d := fileDrop{grant: &fileGrantLine{kind: "group permission", slugs: []string{slug}},
			keep: func(*spawnProfileJSON) { exp.Group.Permissions = append(exp.Group.Permissions, p) }}
		if !spawnGivesPermission("", slug) {
			d.item = permissionItem("group-permission:"+slug, "group permission "+slug, slug, p.ScopeJSON)
		}
		drops = append(drops, d)
	}
	denies := exp.Permissions[:0:0]
	for _, p := range exp.Permissions {
		if p.Effect == db.PermEffectDeny {
			denies = append(denies, p)
			continue
		}
		slug := db.CanonicalPermissionSlug(p.Slug)
		name := holder(p.ConvID)
		d := fileDrop{grant: &fileGrantLine{kind: "permission", holder: name, slugs: []string{slug}},
			keep: func(*spawnProfileJSON) { exp.Permissions = append(exp.Permissions, p) }}
		if !spawnGivesPermission("", slug) {
			d.item = permissionItem("permission:"+p.ConvID+":"+slug, "permission "+slug+" for "+name, slug, p.ScopeJSON)
		}
		drops = append(drops, d)
	}
	exp.Permissions = denies

	// Sudo is temporary, so it is never offered. An older file can still hold it.
	var sudo fileGrants
	for _, s := range exp.SudoGrants {
		if fileSudoGrantLive(s) {
			sudo.add("sudo grant", holder(s.ConvID), s.Slug)
		}
	}
	exp.SudoGrants = nil
	if len(sudo) > 0 {
		drops = append(drops, fileDrop{warning: "This file holds " + strings.Join(sudo.lines(), "; ") +
			". Sudo is temporary, so it does not travel and none of it is applied."})
	}

	for i := range exp.CronJobs {
		job := &exp.CronJobs[i]
		if job.Enabled != 0 {
			drops = append(drops, fileDrop{
				item: importItem{ID: "cron:" + job.Name, Kind: importKindCron, Label: "enabled scheduled job " + job.Name,
					Gives: "Lands enabled, so it starts running on its schedule. It does not run as the operator unless you set that."},
				keep: func(*spawnProfileJSON) { job.Enabled = 1 },
				list: "cron", name: job.Name,
			})
		}
		if job.OperatorAuthored {
			drops = append(drops, fileDrop{list: "cron_unowned", name: job.Name})
		}
		job.Enabled, job.RunImmediately, job.OperatorAuthored = 0, 0, false
	}
	return drops
}

// fileAgentPermissionDrops lists the permissions an agent's file holds that a
// spawn into group would not give, and that could land there. keep records the
// slug in kept. carried is what the receiver's carry policy already applies.
// Permission rows in an agent's file have never warned: they are provenance
// until something applies them, and the security line says so.
func fileAgentPermissionDrops(rows []agentbundle.Permission, group string, carried map[string]db.PermissionOverride, kept map[string]bool) []fileDrop {
	// The name only proves a landing group exists: a preview can come first.
	_, wanted := planCarriedPermissions(rows, permissionCarryPolicy{Group: cmp.Or(group, "group"), Enabled: true, AllowSensitive: true})
	var drops []fileDrop
	for _, slug := range slices.Sorted(permissionOverrideSlugs(wanted)) {
		if wanted[slug].Effect != "grant" || spawnGivesPermission(group, slug) {
			continue
		}
		if _, applied := carried[slug]; applied {
			continue
		}
		drops = append(drops, fileDrop{
			item: importItem{ID: "permission:" + slug, Kind: importKindPermissions, Label: "permission " + slug, Gives: permissionGives(slug), Default: true},
			keep: func(*spawnProfileJSON) { kept[slug] = true },
		})
	}
	return drops
}

func permissionOverrideSlugs(m map[string]db.PermissionOverride) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// fileEnvironmentValue is the value a file's environment variable would land
// with: a recorded path resolves as an applied import resolves it.
func fileEnvironmentValue(entry sandboxpolicy.EnvironmentEntry, placeholders []configbundle.Placeholder, values map[string]string) string {
	home, _ := os.UserHomeDir()
	pairs := []string{"${HOME}", home}
	for _, p := range placeholders {
		pairs = append(pairs, "${"+p.Name+"}", cmp.Or(values[p.Name], p.Original))
	}
	return strings.NewReplacer(pairs...).Replace(entry.Value)
}

// handleImportReveal returns one environment variable's value from an uploaded
// file, for a human who asked to see it. A preview never carries a value, so that
// it stays out of payloads and logs; a request like this one is the only way to
// one, and neither its query nor its response is logged.
//
//	POST /v1/import/reveal?item=env:NAME   — the raw archive as the body (CLI)
//	POST /api/import/reveal?item=env:NAME  — a multipart upload (dashboard)
func handleImportReveal(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "reveal a file's environment value") {
		return
	}
	if !importKeepCaller(r) {
		writeError(w, http.StatusForbidden, "auth", "only a human on this node may reveal a file's environment value")
		return
	}
	name, ok := strings.CutPrefix(r.URL.Query().Get("item"), "env:")
	if !ok || name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "item must name an environment variable: env:NAME")
		return
	}
	archive, _, _, ok := readImportUpload(w, r)
	if !ok {
		return
	}
	value, err := revealFileEnvironment(archive, name)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "value": value})
}

func revealFileEnvironment(archive []byte, name string) (string, error) {
	missing := errors.New("this file holds no environment variable " + name)
	if decoded, err := decodeGroupArchive(archive); err == nil {
		for _, entry := range decoded.Export.Group.Environment {
			if entry.Name == name {
				return entry.Value, nil
			}
		}
		return "", missing
	}
	b, err := decodeAgentArchiveBytes(archive)
	if err != nil {
		return "", fmt.Errorf("read the file: %w", err)
	}
	defer func() { _ = b.Close() }()
	var profile spawnProfileJSON
	if err := json.Unmarshal(b.Manifest.Agent.Profile, &profile); err != nil {
		return "", fmt.Errorf("read the file: %w", err)
	}
	for _, entry := range profile.Environment {
		if entry.Name == name {
			return fileEnvironmentValue(entry, b.Manifest.Placeholders, nil), nil
		}
	}
	return "", missing
}

// withImportForwarded marks ctx as a request a federation peer forwarded.
func withImportForwarded(ctx context.Context) context.Context {
	return context.WithValue(ctx, importForwardedKey{}, true)
}
