package agentd

import (
	"encoding"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// config_classes.go classifies every config.json key for remote
// administration (config_remote.go). Each key belongs to exactly one class,
// and each class names the peer grant that writes it, so a grant for one
// class can never be stretched to change another. The table is checked
// against config.Config by reflection: a new key without a class fails the
// test, so a new setting cannot silently become remotely writable or
// unwritable.

// Peer grants for a node's Config tab. All are instance-wide peer grants,
// default off. Every write grant also needs node.config.read, which guards
// the view a write returns.
const (
	PermNodeConfigRead        = "node.config.read"
	PermNodeConfigWrite       = "node.config.write"
	PermNodeConfigAgents      = "node.config.agents"
	PermNodeConfigNetwork     = "node.config.network"
	PermNodeConfigCredentials = "node.config.credentials"
)

// configClass is a key's risk class.
type configClass string

const (
	// configClassGeneral: display, notifications, context, costs, logs: what
	// changes how the node looks and reports, not what it lets anyone do.
	configClassGeneral configClass = "general"
	// configClassAgents: what agents may do or hand themselves (default
	// permissions, sudo, spawn guardrails, steward powers, sandbox choices,
	// inbound teleports) and settings that delete data.
	configClassAgents configClass = "agents"
	// configClassNetwork: where the dashboard listens, operator tokens,
	// remote access, the route data plane and federation tunables.
	configClassNetwork configClass = "network"
	// configClassCredentials: the credential proxies (forges, Linear, awb,
	// HTTP proxies) and the keys and tokens they use.
	configClassCredentials configClass = "credentials"
	// configClassInstallation: how tclaude is installed, updated and run as
	// a service. A restricted peer changes these through their own
	// endpoints under node.update.settings and its restricted rules
	// (node_update_settings.go, node_service_launch.go), never here.
	configClassInstallation configClass = "installation"
	// configClassLocal: shell commands agentd runs, the hub link and trust,
	// accepting remote scripts, and the audit retention. Only this node's
	// own operator changes them: no peer does, unrestricted included, and no
	// grant opens them.
	configClassLocal configClass = "local"
)

// configClasses lists every class in display order.
var configClasses = []configClass{configClassGeneral, configClassAgents, configClassNetwork, configClassCredentials, configClassInstallation, configClassLocal}

// configClassSlug is the peer grant that writes a class; a class without one
// is the node's own (local) or unrestricted trust's (installation).
var configClassSlug = map[configClass]string{
	configClassGeneral:     PermNodeConfigWrite,
	configClassAgents:      PermNodeConfigAgents,
	configClassNetwork:     PermNodeConfigNetwork,
	configClassCredentials: PermNodeConfigCredentials,
}

// configKeyClasses maps a dotted config path prefix to its class; the longest
// matching prefix wins. Keep the entries grouped by top-level key.
var configKeyClasses = []struct {
	prefix string
	class  configClass
}{
	{"status_snapshot", configClassGeneral},
	{"host", configClassGeneral},
	// The directories offered as spawn roots.
	{"host.work_dirs", configClassAgents},
	{"capacity", configClassAgents},
	{"notifications", configClassGeneral},
	// A shell command agentd runs for each notification.
	{"notifications.notification_command", configClassLocal},
	{"pre_compact_guard", configClassGeneral},
	{"log_level", configClassGeneral},
	{"startup_timing", configClassGeneral},
	{"record_hooks", configClassGeneral},
	{"ratelimit", configClassGeneral},

	{"agent", configClassAgents},
	{"agent.context_nudge", configClassGeneral},
	{"agent.auto_launch_dashboard", configClassGeneral},
	{"agent.access_request_auto_open_browser", configClassGeneral},
	{"agent.access_request_system_notification", configClassGeneral},
	{"agent.present_pr_notification", configClassGeneral},
	{"agent.disable_tray", configClassGeneral},
	{"agent.branch_history_pr_enrichment", configClassGeneral},
	{"agent.morph", configClassGeneral},
	{"agent.spawn_name_normalize", configClassGeneral},
	{"agent.spawn_label_from_name", configClassGeneral},
	{"agent.spawn_inline_max_chars", configClassGeneral},
	{"agent.message_inline_max_chars", configClassGeneral},
	{"agent.dashboard_port", configClassNetwork},
	{"agent.dashboard_bind", configClassNetwork},
	{"agent.persist_operator_token", configClassNetwork},
	{"agent.persist_operator_token_keychain", configClassNetwork},
	{"agent.git_proxy", configClassCredentials},
	{"agent.linear_proxy", configClassCredentials},
	{"agent.awb_proxy", configClassCredentials},
	{"agent.http_proxies", configClassCredentials},
	{"agent.model_proxy_disabled", configClassCredentials},

	{"experimental_harnesses", configClassAgents},
	{"broker", configClassAgents},
	{"terminal", configClassGeneral},
	{"log_rotation", configClassGeneral},
	{"focus", configClassGeneral},
	{"slop", configClassGeneral},
	{"conv_watch", configClassGeneral},
	{"session_watch", configClassGeneral},
	{"cost", configClassGeneral},
	{"opencode", configClassGeneral},
	{"ask", configClassGeneral},
	{"ask.sandbox", configClassAgents},
	// The scribe and the Steward are agents this node spawns: their profile,
	// directory and permissions are what they may do.
	{"scribe", configClassAgents},
	{"steward", configClassAgents},
	// A peer must not shorten the trail of its own actions.
	{"audit", configClassLocal},
	{"remote_access", configClassNetwork},

	{"federation", configClassNetwork},
	{"federation.board_update_notify", configClassGeneral},
	{"federation.health", configClassGeneral},
	{"federation.teleport", configClassAgents},
	{"federation.max_live_agents", configClassAgents},
	{"federation.unrestricted_max_live", configClassAgents},
	{"federation.away", configClassAgents},
	// Accepting remote scripts is running a peer's code here.
	{"federation.scripts", configClassLocal},
	// The hub link and the node's identity are federation trust.
	{"federation.enabled", configClassLocal},
	{"federation.hub_url", configClassLocal},
	{"federation.invite", configClassLocal},
	{"federation.hub_ca_file", configClassLocal},
	{"federation.identity_rotation_seconds", configClassLocal},

	{"updates", configClassInstallation},
	// Shell commands, and what the restricted self-update rules keep local.
	{"updates.scripts", configClassLocal},
	{"updates.harnesses", configClassLocal},
	{"updates.self.custom", configClassLocal},
	{"updates.self.source_checkout", configClassLocal},
	{"updates.self.install_dir", configClassLocal},
	{"updates.self.package_manager_ack", configClassLocal},
	{"service", configClassInstallation},

	// Retention deletes snapshots past it.
	{"snapshots", configClassAgents},
	{"routes", configClassNetwork},
	{"claude_resume", configClassGeneral},
	// Claude Code deletes transcripts older than this.
	{"claude_cleanup_period_days", configClassAgents},
	{"processes", configClassAgents},
	{"taskboard", configClassGeneral},
	// Retention deletes done tasks and their history.
	{"taskboard.archive_after_days", configClassAgents},
	{"taskboard.retention_days", configClassAgents},
	{"dashboard", configClassGeneral},
	{"session", configClassGeneral},
	// Joining a group hands an agent the group's grants.
	{"session.auto_join_group", configClassAgents},
	{"session.auto_join_or_create_group", configClassAgents},
	{"tui", configClassGeneral},
	{"usage", configClassGeneral},
	{"features", configClassGeneral},
	{"features.agent_dirs_mount_parent", configClassAgents},
}

// configSecretPaths are the values that never leave this node, not even for
// an unrestricted peer: tokens, header values, and free-text commands that
// may embed them. A peer sees each one empty, with whether it is set; it may
// replace one but never read it. A segment "*" is every key of a map, and
// "[field]" is every element of a list, named by its field.
var configSecretPaths = []string{
	"federation.invite",
	"agent.http_proxies.*.header_value",
	"service.launch.env[key].value",
	"notifications.notification_command",
	"updates.scripts[name].status",
	"updates.scripts[name].update",
	"updates.harnesses.*.update",
	"updates.self.custom.command",
	// Serve flags may carry a token (--token=…); the service endpoints show
	// them with only the secret ones withheld.
	"service.launch.flags",
	"service.launcher.serve_args",
}

// configCredentialBindings tie a credential to where it is sent. No peer,
// unrestricted included, sets a credential's source file (it could point one
// at any file this node can read): it keeps or removes one the node set. No
// peer changes where a credential it did not send goes (it could send a kept
// one to its own server): a new destination needs the secret value sent
// again, or a file credential changed on the node itself.
var configCredentialBindings = []struct {
	object  string   // dotted path; "*" is every key of a map
	secrets []string // secret value fields (configSecretPaths)
	files   []string // credential file fields
	dests   []string // where the credential goes
}{
	{"agent.http_proxies.*", []string{"header_value"}, []string{"header_value_file"}, []string{"url", "header", "environment_variable"}},
	{"agent.awb_proxy", nil, []string{"password_file"}, []string{"url", "username"}},
	{"agent.git_proxy", nil, []string{"ssh_key", "github_token_file", "gitlab_token_file"}, []string{"gitlab_hosts"}},
	{"agent.linear_proxy", nil, []string{"api_key_file"}, nil},
}

// configCredentialFileLists are list leaves whose elements name credential
// files: a peer keeps or removes those the node set, and adds none.
var configCredentialFileLists = map[string]string{
	"agent.linear_proxy.workspaces": "api_key_file",
}

// configKeyClass returns the class of a dotted leaf path.
func configKeyClass(path string) (configClass, bool) {
	best, class := -1, configClass("")
	for _, e := range configKeyClasses {
		if (path == e.prefix || strings.HasPrefix(path, e.prefix+".")) && len(e.prefix) > best {
			best, class = len(e.prefix), e.class
		}
	}
	return class, best >= 0
}

var configLeaves = sync.OnceValue(func() []string {
	var out []string
	walkConfigLeaves(reflect.TypeFor[config.Config](), "", map[reflect.Type]bool{}, &out)
	slices.Sort(out)
	return out
})

// configLeafPaths are the dotted paths of config.Config's leaves: each
// non-struct field, with a list or map counted whole.
func configLeafPaths() []string { return configLeaves() }

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

func walkConfigLeaves(t reflect.Type, prefix string, seen map[reflect.Type]bool, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// A struct with its own JSON form (time.Time) is one value.
	if t.Kind() != reflect.Struct || seen[t] || t.Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(jsonMarshalerType) ||
		t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType) {
		*out = append(*out, prefix)
		return
	}
	seen[t] = true
	defer delete(seen, t)
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			if f.Anonymous {
				walkConfigLeaves(f.Type, prefix, seen, out)
			}
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		walkConfigLeaves(f.Type, path, seen, out)
	}
}

// configKeyInfo is one row of the classification table the UI receives.
type configKeyInfo struct {
	Path   string      `json:"path"`
	Class  configClass `json:"class"`
	Secret bool        `json:"secret,omitempty"`
}

var configKeyTable = sync.OnceValue(func() []configKeyInfo {
	var out []configKeyInfo
	for _, path := range configLeafPaths() {
		class, _ := configKeyClass(path)
		secret := slices.ContainsFunc(configSecretPaths, func(p string) bool { return configSecretPathRoot(p) == path })
		out = append(out, configKeyInfo{Path: path, Class: class, Secret: secret})
	}
	return out
})

// configSecretPathRoot is the leaf a secret path lies in: its prefix up to
// the first map or list segment.
func configSecretPathRoot(p string) string {
	segs := strings.Split(p, ".")
	for i, s := range segs {
		if s == "*" {
			return strings.Join(segs[:i], ".")
		}
		if j := strings.IndexByte(s, '['); j >= 0 {
			return strings.Join(append(segs[:i:i], s[:j]), ".")
		}
	}
	return p
}

// secretSite is one place a secret value sits in a decoded config tree.
type secretSite struct {
	path      string
	leaf      string
	parent    map[string]any
	key       string
	ambiguous bool
}

func (s secretSite) value() any {
	if s.parent == nil {
		return nil
	}
	return s.parent[s.key]
}

// configSecretSites lists every place a secret value can sit in tree,
// including a leaf key its parent omits.
func configSecretSites(tree map[string]any) []secretSite {
	var out []secretSite
	for _, pattern := range configSecretPaths {
		collectSecretSites(tree, strings.Split(pattern, "."), "", configSecretPathRoot(pattern), &out)
	}
	return out
}

func collectSecretSites(node map[string]any, segs []string, prefix, leaf string, out *[]secretSite) {
	join := func(s string) string {
		if prefix == "" {
			return s
		}
		return prefix + "." + s
	}
	seg := segs[0]
	if len(segs) == 1 {
		*out = append(*out, secretSite{path: join(seg), leaf: leaf, parent: node, key: seg})
		return
	}
	if seg == "*" {
		for k, v := range node {
			if child, ok := v.(map[string]any); ok {
				collectSecretSites(child, segs[1:], join(k), leaf, out)
			}
		}
		return
	}
	if name, field, ok := strings.Cut(seg, "["); ok {
		field = strings.TrimSuffix(field, "]")
		// Elements go by their field. A repeated or empty one names no
		// element for sure: it is marked ambiguous, and a peer change of a
		// config that holds one is refused.
		list, _ := node[name].([]any)
		seen := map[string]bool{}
		for _, el := range list {
			child, ok := el.(map[string]any)
			if !ok {
				continue
			}
			id, _ := child[field].(string)
			if id == "" || seen[id] {
				*out = append(*out, secretSite{path: join(name + "[" + id + "]"), leaf: leaf, ambiguous: true})
				continue
			}
			seen[id] = true
			collectSecretSites(child, segs[1:], join(name+"["+id+"]"), leaf, out)
		}
		return
	}
	if child, ok := node[seg].(map[string]any); ok {
		collectSecretSites(child, segs[1:], join(seg), leaf, out)
	}
}

// secretValueSet reports whether a secret value is present: a non-empty
// string or list.
func secretValueSet(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	default:
		return true
	}
}
