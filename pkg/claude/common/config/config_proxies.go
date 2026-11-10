package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// AWBProxyConfig is the operator's policy for the daemon-mediated AWB proxy —
// Agent Work Board, an agent-first issue tracker with an HTTP API.
//
// It answers the same question LinearProxyConfig does, in AWB's vocabulary:
// which WORKSPACES may agentd reach with the operator's AWB account, on behalf of
// an agent that holds no credentials of its own?
//
// AWB has no filesystem artifact that could anchor an agent the way a git work
// tree anchors the git proxy. A workspace allow-list is therefore the whole scope
// gate, which is why it is fail-closed. This block is the OPERATOR-GLOBAL half;
// the per-agent half is an `awb_workspace` scope on the agent's own
// proxy.awb.read / proxy.awb.write grant (`tclaude agent permissions grant
// <agent> proxy.awb.read --scope awb_workspace=awb`). The two are enforced
// together and neither can widen the other.
//
// Unlike Linear there is no multi-key routing here: one URL is one server, and
// one account reaches every workspace it is a member of. An operator running two
// AWB servers points the daemon at one of them.
type AWBProxyConfig struct {
	// URL is the base URL of the AWB server's HTTP API — "https://awb.example"
	// or "https://example/awb" when a reverse proxy serves it under a path.
	// The proxy joins "/api/…" onto it.
	//
	// It is what OPTS THE OPERATOR IN: with no URL the daemon has no server to
	// call, so `tclaude proxy awb` is not registered and its permission slugs
	// are hidden. Only http:// and https:// are accepted, and a URL carrying
	// userinfo is refused — credentials belong in Username and PasswordFile,
	// where they do not travel in log lines.
	URL string `json:"url,omitempty"`

	// Username is the AWB account every proxied call authenticates as, and
	// therefore the identity AWB attributes every write to. It is also what
	// `claim` records as the assignee when the caller gives no --as, and what
	// `--mine` filters on.
	//
	// Empty means the server holds no users and authenticates nothing, which
	// AWB supports; the daemon then sends no credentials. That is only sensible
	// for a server nothing but the daemon can reach.
	Username string `json:"username,omitempty"`

	// PasswordFile is a file whose contents become the AWB password. When empty
	// the daemon falls back to AWB_PASSWORD in its own environment; with
	// neither, and a Username set, the proxy refuses rather than sending a
	// half-formed credential.
	//
	// The password is deliberately NOT a field of this struct's own JSON: this
	// file is plaintext, shows up in the dashboard's Config tab, and is the
	// sort of thing an operator copies into a bug report. Same treatment as
	// LinearProxyConfig.APIKeyFile — `~/…` is expanded against the home
	// directory of the account agentd runs as, and shell variables are not.
	PasswordFile string `json:"password_file,omitempty"`

	// AllowedWorkspaces is the allow-list of AWB workspace keys the proxy may act
	// on — the prefix of an issue ID, so "awb" authorizes awb-a3f9c1.
	// Compared case-insensitively.
	//
	// EMPTY OR ABSENT DISABLES UNSCOPED GRANTS, exactly as an empty
	// AllowedTeams disables unscoped Linear-proxy grants: an agent whose grant
	// carries an `awb_workspace` scope supplies its own workspaces instead, and
	// when both exist a request must satisfy both. There is deliberately no
	// wildcard: workspace keys are a flat namespace with no hierarchy to match a
	// prefix against, so a wildcard would only ever mean "all of them".
	AllowedWorkspaces []string `json:"allowed_workspaces,omitempty"`
	// LegacyAllowedProjects reads and preserves configs written before AWB
	// renamed projects to workspaces. Effective policy merges it into
	// AllowedWorkspaces; remove after one compatibility cycle.
	LegacyAllowedProjects []string `json:"allowed_projects,omitempty"`

	// AllowWrite permits the mutating verbs (create, update, claim, release,
	// close, reopen, delete, label, dep add/rm, attach add/delete) at all.
	// Default off, so an operator who wants an agent to READ its ticket does
	// not silently also let it write to the tracker under their account. The
	// `proxy.awb.write` permission slug still gates the caller on top of this:
	// the config is the operator's ceiling, the slug is the per-agent grant.
	AllowWrite bool `json:"allow_write,omitempty"`

	// ReadyPolling opts agentd into dispatching AWB's first matching ready
	// issue. Keys are process names; each entry names its AWB workspace and one
	// serial worker is started for each entry.
	ReadyPolling map[string]AWBReadyPollingConfig `json:"ready_polling,omitempty"`
}

// AWBReadyPollingConfig describes one named ready-issue polling process.
// Interval is a duration string so config files remain readable; an omitted
// value resolves to one minute.
type AWBReadyPollingConfig struct {
	Workspace      string      `json:"workspace"`
	Labels         []string    `json:"labels,omitempty"`
	Group          string      `json:"group"`
	Cwd            string      `json:"cwd"`
	Interval       string      `json:"interval,omitempty"`
	Profile        string      `json:"profile,omitempty"`
	SandboxProfile string      `json:"sandbox_profile,omitempty"`
	Harness        HarnessList `json:"harness,omitempty"`
	Model          string      `json:"model,omitempty"`
	Effort         string      `json:"effort,omitempty"`
	Worktree       bool        `json:"worktree,omitempty"`
	MonitorPR      bool        `json:"monitor_pr,omitempty"`
	MonitorCommit  bool        `json:"monitor_commit,omitempty"`
	MonitorClose   bool        `json:"monitor_close,omitempty"`
	SkipEpics      bool        `json:"skip_epics,omitempty"`
}

// HarnessList is a harness preference written as either one harness or an
// ordered list of them. A list is a fallback chain: the daemon takes the first
// entry whose subscription usage is still under the operator's `ratelimit`
// ceilings, so a process can keep working on a second vendor while the first
// one's window recovers.
//
// It accepts both JSON shapes so an existing `"harness": "codex"` keeps
// working unchanged, and marshals a single entry back to a plain string so an
// unrelated config save cannot silently rewrite an operator's file into a
// shape they did not choose.
type HarnessList []string

func (h *HarnessList) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*h = nil
		return nil
	}
	if trimmed[0] == '[' {
		var list []string
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		*h = list
		return nil
	}
	var one string
	if err := json.Unmarshal(data, &one); err != nil {
		return fmt.Errorf("harness must be a string or a list of strings: %w", err)
	}
	*h = HarnessList{one}
	return nil
}

func (h HarnessList) MarshalJSON() ([]byte, error) {
	switch len(h) {
	case 0:
		return []byte("null"), nil
	case 1:
		return json.Marshal(h[0])
	default:
		return json.Marshal([]string(h))
	}
}

// LinearProxyConfig is the operator's policy for the daemon-mediated Linear
// proxy. It answers the same question GitProxyConfig does, in Linear's
// vocabulary: which teams may agentd reach with the operator's API key, on
// behalf of an agent that holds no key of its own?
//
// Linear has no filesystem artifact that could anchor an agent the way a git
// work tree anchors the git proxy — no `.git/config` equivalent ties a
// conversation to an issue. A team allow-list is therefore the whole scope
// gate, which is why it is mandatory and fail-closed.
//
// This block is the OPERATOR-GLOBAL half of that gate. The per-agent half is a
// `linear_team` scope on the agent's own proxy.linear.read / proxy.linear.write grant
// (`tclaude agent permissions grant <agent> proxy.linear.read --scope
// linear_team=TCL`), which narrows what that one agent may reach. The two are
// enforced together and neither can widen the other.
type LinearProxyConfig struct {
	// AllowedTeams is the allow-list of Linear team keys the proxy may act
	// on — the short prefix in an issue identifier, so "TCL" authorizes
	// TCL-1, TCL-568 and so on. Compared case-insensitively.
	//
	// EMPTY OR ABSENT DISABLES UNSCOPED GRANTS, exactly as an empty
	// AllowedRemotes disables unscoped git-proxy grants: an agent whose grant
	// carries a `linear_team` scope supplies its own teams instead, and when
	// both exist a request must satisfy both. There is deliberately no
	// wildcard and no "allow every team" setting: team keys are a flat
	// namespace with no hierarchy to match a prefix against, so a wildcard
	// would only ever mean "all of them" — which is the setting an operator
	// should have to write out team by team.
	AllowedTeams []string `json:"allowed_teams,omitempty"`

	// APIKeyFile is a file whose contents become the Linear personal API
	// key. When empty the daemon falls back to LINEAR_API_KEY in its own
	// environment; with neither, the proxy reports itself unconfigured and
	// refuses. The key is never written to config.json itself — that file is
	// plaintext and shows up in the dashboard's Config tab and in backups.
	//
	// Accepts `~/…`, expanded against the home directory of the account
	// agentd runs as. Shell variables are NOT expanded — a config file is
	// not a shell.
	//
	// This is the DEFAULT key: it is used for every allowed team that no
	// Workspaces entry claims. An operator whose teams all live in one Linear
	// workspace needs nothing else.
	APIKeyFile string `json:"api_key_file,omitempty"`

	// Workspaces routes named teams to a DIFFERENT key from APIKeyFile.
	//
	// It exists because a Linear personal API key is scoped to ONE workspace:
	// the one its creator was logged into. Within that workspace a single key
	// reaches every team its account can see, whoever created them — so teams
	// that merely have different owners need no entry here. Teams in a
	// SEPARATE workspace cannot be reached by that key at all, at which point
	// the only way to cover them is a second key created in that workspace.
	//
	// Authorization is NOT what this list does. AllowedTeams above and the
	// caller's grant scope decide which teams may be reached; an entry here
	// only says which credential reaches one. A team named here and absent
	// from AllowedTeams stays unreachable, and adding a workspace can never
	// widen what an agent may touch.
	Workspaces []LinearWorkspaceConfig `json:"workspaces,omitempty"`

	// AllowWrite permits the mutating verbs (`issue create`, `issue
	// comment`, `issue update`, `issue link`) at all. Default off, so an
	// operator who wants an agent to READ its ticket does not silently also
	// let it write to the workspace under their name. The `proxy.linear.write`
	// permission slug still gates the caller on top of this: the config is
	// the operator's ceiling, the slug is the per-agent grant.
	AllowWrite bool `json:"allow_write,omitempty"`
}

// LinearWorkspaceConfig binds one Linear workspace's key to the teams that
// live in it — one entry per EXTRA workspace beyond the one
// LinearProxyConfig.APIKeyFile belongs to.
//
// Both fields are required, and a team key may appear in at most one entry. The
// daemon refuses to serve a policy that breaks either rule rather than picking
// a key for an ambiguously-routed team: a request answered by the wrong
// workspace's key does not fail cleanly, it reports the issue as missing.
//
// Note that Linear only guarantees a team key to be unique WITHIN a workspace,
// so two workspaces can each have an "OPS". Nothing in this proxy can tell
// those apart — the allow-list, the `linear_team` grant scope and the issue
// identifier all carry the bare key — so colliding keys across workspaces are
// unsupported rather than merely unrouted.
type LinearWorkspaceConfig struct {
	// Name labels the workspace in diagnostics — `whoami`'s per-workspace
	// breakdown, and the refusal an unreadable key produces. Free text, and
	// only that: nothing routes on it. Blank falls back to the entry's
	// position ("workspace 2").
	Name string `json:"name,omitempty"`

	// APIKeyFile is a file holding the personal API key created IN THIS
	// WORKSPACE. Same handling as LinearProxyConfig.APIKeyFile: `~/…` is
	// expanded, shell variables are not, and the key never enters
	// config.json. Unlike that field there is no environment fallback — one
	// LINEAR_API_KEY cannot stand in for several workspaces, so an entry
	// without a file names no key at all and is refused.
	APIKeyFile string `json:"api_key_file"`

	// Teams is the team keys this workspace's key reaches, compared
	// case-insensitively. Empty makes the entry unreachable, which is a
	// misconfiguration rather than a no-op, so it is refused too.
	Teams []string `json:"teams"`
}

// GitProxyConfig is the operator's policy for the daemon-mediated Git-remote
// and GitHub proxy. It answers one question: which remotes may agentd reach
// with the operator's credentials, on behalf of an agent that cannot reach
// them itself?
//
// It deliberately does NOT decide WHERE an operation may run. That is the git
// work tree containing the agent's own daemon-recorded physical launch
// directory, and it is not operator-tunable — see agentd.resolveProxyRepo for
// exactly what is checked. The proxy lends credentials, never filesystem reach.
type GitProxyConfig struct {
	// AllowedRemotes is the allow-list of remotes the proxy may talk to,
	// written as slash-separated `host/owner/repo` patterns matched
	// case-insensitively against the remote's resolved URL:
	//
	//   "github.com"                  every repo on github.com
	//   "github.com/tofutools"        every repo in that owner
	//   "github.com/tofutools/*"      the same, spelled explicitly
	//   "github.com/tofutools/myrepo" exactly one repo
	//
	// A `*` matches exactly one segment. A pattern with fewer segments than
	// the target matches as a prefix, so "github.com" covers everything on
	// that host.
	//
	// EMPTY OR ABSENT disables unscoped proxy grants. A grant carrying the
	// remote scope can authorize its own remote patterns instead; when both
	// mechanisms are present they are enforced together.
	AllowedRemotes []string `json:"allowed_remotes,omitempty"`

	// ProtectedRefs are branch names the proxy refuses to push to at all,
	// force or not. Absent uses DefaultGitProxyProtectedRefs ("main",
	// "master"); an explicit empty list turns the protection off. Entries
	// are branch-name patterns where `*` matches within one segment, so
	// "release/*" protects a whole namespace.
	//
	// This is a blunt, deliberately non-negotiable guard: the point of the
	// proxy is to let an agent do ordinary feature work, and an agent
	// pushing straight onto the trunk is the failure mode with the worst
	// blast radius.
	ProtectedRefs *[]string `json:"protected_refs,omitempty"`

	// AllowForcePush permits `--force-with-lease` on non-protected refs.
	// Default off. Protected refs are never force-pushable regardless.
	AllowForcePush bool `json:"allow_force_push,omitempty"`

	// SSHKey optionally pins a single private key for the proxy's SSH
	// transport (rendered as `ssh -i <key> -o IdentitiesOnly=yes`). Empty
	// uses the daemon's ambient SSH setup — normally an ssh-agent, which is
	// the preferred posture because no secret then enters the child process
	// at all. The path is the KEY, not a passphrase; the proxy always runs
	// ssh with BatchMode=yes, so a passphrase-protected key that is not
	// already loaded into an agent fails fast instead of hanging the daemon
	// on a prompt.
	SSHKey string `json:"ssh_key,omitempty"`

	// GitHubTokenFile optionally names a file whose contents are the GitHub
	// token handed to `gh` as GH_TOKEN. Empty lets `gh` use the daemon's own
	// authenticated configuration. The token is passed through the child's
	// environment or this file — never through argv, which is world-readable
	// for the process lifetime via /proc/<pid>/cmdline.
	GitHubTokenFile string `json:"github_token_file,omitempty"`

	// GitLabHosts maps a remote HOST (as it appears in the allow-list and in
	// the agent's remote URL, without a port) to the base URL of the GitLab
	// instance that serves it:
	//
	//   "gitlab_hosts": {"gitlab.example.com": "https://gitlab.example.com:8929"}
	//
	// The map does two jobs. It is how the daemon RECOGNISES a remote as a
	// GitLab project — a self-hosted instance cannot be told apart from any
	// other git server by its name — and it is where the web/API base lives,
	// which may differ from the SSH host in scheme and port. gitlab.com needs
	// no entry. The base must be https; the token is only ever sent to it.
	GitLabHosts map[string]string `json:"gitlab_hosts,omitempty"`

	// GitLabTokenFile names a file whose contents are the GitLab personal or
	// project access token the GitLab proxy sends as an Authorization: Bearer
	// header. There is no CLI fallback: with no file configured the GitLab
	// proxy refuses every call and says so. One token for every configured host.
	GitLabTokenFile string `json:"gitlab_token_file,omitempty"`

	// gitlabHostsListed records that the operator listed instances in
	// gitlab_hosts, even if normalisation dropped every one of them. It is
	// what keeps the gitlab.com default off: a list whose entries were all
	// invalid is still a statement that the token is not gitlab.com's.
	gitlabHostsListed bool
}

// defaultGitLabHosts are the GitLab instances recognised without a
// gitlab_hosts entry.
var defaultGitLabHosts = map[string]string{"gitlab.com": "https://gitlab.com"}

// GitLabBase returns the web/API base URL for a remote host that is a known
// GitLab instance, or "" when the host is not one. The host is matched
// case-insensitively and without a port, which is how the allow-list spells
// it too.
//
// The built-in gitlab.com applies only while gitlab_hosts is empty. The one
// gitlab_token_file goes to every instance this recognises. Once the operator
// lists an instance, the token is that instance's, and gitlab.com, a third
// party, must be listed too before it is treated as GitLab and receives the
// token.
func (p GitProxyConfig) GitLabBase(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return ""
	}
	if base, ok := p.GitLabHosts[host]; ok {
		return base
	}
	if len(p.GitLabHosts) > 0 || p.gitlabHostsListed {
		return ""
	}
	return defaultGitLabHosts[host]
}

// GitLabBases lists the base URL of every GitLab instance the policy
// recognises — the configured gitlab_hosts plus the defaults they do not
// override — sorted, so a caller that derives configuration from it is
// deterministic. These are exactly the bases GitLabBase can answer with, and
// so the only places the GitLab token is ever sent.
func (p GitProxyConfig) GitLabBases() []string {
	seen := map[string]bool{}
	var out []string
	add := func(base string) {
		if base != "" && !seen[base] {
			seen[base] = true
			out = append(out, base)
		}
	}
	for host := range p.GitLabHosts {
		add(p.GitLabBase(host))
	}
	for host := range defaultGitLabHosts {
		add(p.GitLabBase(host))
	}
	sort.Strings(out)
	return out
}

// DefaultGitProxyProtectedRefs are the branches the Git proxy refuses to push
// to when the operator has not said otherwise.
var DefaultGitProxyProtectedRefs = []string{"main", "master"}

// ResolvedGitProxy returns the effective Git-proxy policy: the configured
// block with absent fields filled in, and every pattern trimmed and
// lower-cased. It is nil-safe — a nil config, absent agent block, or absent
// git_proxy block all yield a zero policy whose empty AllowedRemotes means
// "proxy disabled".
func (c *Config) ResolvedGitProxy() GitProxyConfig {
	var out GitProxyConfig
	if c != nil && c.Agent != nil && c.Agent.GitProxy != nil {
		src := c.Agent.GitProxy
		out.AllowedRemotes = normalizeGitProxyPatterns(src.AllowedRemotes)
		out.AllowForcePush = src.AllowForcePush
		out.SSHKey = strings.TrimSpace(src.SSHKey)
		out.GitHubTokenFile = strings.TrimSpace(src.GitHubTokenFile)
		out.GitLabTokenFile = strings.TrimSpace(src.GitLabTokenFile)
		out.GitLabHosts = normalizeGitLabHosts(src.GitLabHosts)
		out.gitlabHostsListed = len(src.GitLabHosts) > 0
		if src.ProtectedRefs != nil {
			// An explicit list — including an explicit empty one, which
			// deliberately turns the protection off — wins over the default.
			refs := normalizeGitProxyPatterns(*src.ProtectedRefs)
			out.ProtectedRefs = &refs
			return out
		}
	}
	refs := append([]string(nil), DefaultGitProxyProtectedRefs...)
	out.ProtectedRefs = &refs
	return out
}

// GitProxyEnabled reports whether the operator has opted into the proxy at
// all. Everything else about the feature is gated behind this, so a daemon
// with no git_proxy block never runs `git` or `gh` on an agent's behalf.
func (c *Config) GitProxyEnabled() bool {
	return len(c.ResolvedGitProxy().AllowedRemotes) > 0
}

// ResolvedLinearProxy returns the effective Linear-proxy policy. Nil-safe in
// the same way ResolvedGitProxy is: a nil config, absent agent block, or
// absent linear_proxy block all yield a zero policy whose empty AllowedTeams
// means "proxy disabled".
//
// Team keys go through normalizeGitProxyPatterns despite the name: it trims,
// de-blanks, de-duplicates and lower-cases, which is exactly the treatment a
// team key needs, and the matcher lower-cases the other side too.
func (c *Config) ResolvedLinearProxy() LinearProxyConfig {
	var out LinearProxyConfig
	if c != nil && c.Agent != nil && c.Agent.LinearProxy != nil {
		src := c.Agent.LinearProxy
		out.AllowedTeams = normalizeGitProxyPatterns(src.AllowedTeams)
		out.APIKeyFile = strings.TrimSpace(src.APIKeyFile)
		out.AllowWrite = src.AllowWrite
		out.Workspaces = normalizeLinearWorkspaces(src.Workspaces)
	}
	return out
}

// normalizeLinearWorkspaces trims each workspace entry and normalizes its team
// list the way AllowedTeams is normalized, so the two are directly comparable.
//
// It deliberately does NOT drop malformed entries. An entry with no key file or
// no teams is a mistake the operator has to see: silently discarding it would
// route that workspace's teams to the default key, which is exactly the wrong
// key, and the resulting "issue does not exist" would send them looking at
// Linear rather than at their config. The daemon refuses such a policy — see
// agentd.linearRoutes.
func normalizeLinearWorkspaces(in []LinearWorkspaceConfig) []LinearWorkspaceConfig {
	if len(in) == 0 {
		return nil
	}
	out := make([]LinearWorkspaceConfig, 0, len(in))
	for _, ws := range in {
		out = append(out, LinearWorkspaceConfig{
			Name:       strings.TrimSpace(ws.Name),
			APIKeyFile: strings.TrimSpace(ws.APIKeyFile),
			Teams:      normalizeGitProxyPatterns(ws.Teams),
		})
	}
	return out
}

// LinearProxyEnabled reports whether the operator has opted into the Linear
// proxy OPERATOR-GLOBALLY. It answers a question about this config block alone,
// so it is not the whole enablement rule: an agent whose grant carries a
// `linear_team` scope is reachable with no allowed_teams list at all. The
// daemon's own gate is agentd.linearEffectiveTeams, which consults both.
func (c *Config) LinearProxyEnabled() bool {
	return len(c.ResolvedLinearProxy().AllowedTeams) > 0
}

// LinearProxyConfigured reports whether the operator has set up the Linear
// proxy AT ALL — any allow-list, any key file, any workspace route.
//
// It is deliberately broader than LinearProxyEnabled, and answers a different
// question. LinearProxyEnabled is about the operator's global TEAM POLICY, one
// half of an authorization decision. This is about whether the feature exists
// on this host, which is what the permission catalog needs: a slug hidden from
// the catalog is one an operator cannot grant, so the test for showing it has
// to be "could this ever work here" rather than "is it fully configured".
//
// A key supplied only through LINEAR_API_KEY in the daemon's environment is
// invisible from here; the caller adds that signal, since reading the daemon's
// own environment is not config's job.
func (c *Config) LinearProxyConfigured() bool {
	p := c.ResolvedLinearProxy()
	return len(p.AllowedTeams) > 0 || p.APIKeyFile != "" || len(p.Workspaces) > 0
}

// LinearTeamAllowed reports whether key names a team the operator allow-listed.
// Exact, case-insensitive match on the whole key — there is no prefix or
// wildcard rule here, unlike the remote matcher, because team keys are a flat
// namespace: a prefix match would let "TCL" authorize "TCLX". The `linear_team`
// permission-scope matcher deliberately uses the same rule.
//
// This is the operator half of the gate only. A request is authorized by
// agentd's effective team set, which also folds in the caller's grant scope.
func (p LinearProxyConfig) LinearTeamAllowed(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return false
	}
	return slices.Contains(p.AllowedTeams, key)
}

// ResolvedAWBProxy returns the effective AWB-proxy policy. Nil-safe in the same
// way ResolvedLinearProxy is: a nil config, absent agent block, or absent
// awb_proxy block all yield a zero policy whose empty URL means "proxy off".
//
// Workspace keys go through normalizeGitProxyPatterns despite the name: it trims,
// de-blanks, de-duplicates and lower-cases, which is exactly the treatment an
// AWB workspace key needs, and the matcher lower-cases the other side too.
func (c *Config) ResolvedAWBProxy() AWBProxyConfig {
	var out AWBProxyConfig
	if c != nil && c.Agent != nil && c.Agent.AWBProxy != nil {
		src := c.Agent.AWBProxy
		out.URL = strings.TrimRight(strings.TrimSpace(src.URL), "/")
		out.Username = strings.TrimSpace(src.Username)
		out.PasswordFile = strings.TrimSpace(src.PasswordFile)
		allowed := append(append([]string{}, src.AllowedWorkspaces...), src.LegacyAllowedProjects...)
		out.AllowedWorkspaces = normalizeGitProxyPatterns(allowed)
		out.AllowWrite = src.AllowWrite
		if len(src.ReadyPolling) > 0 {
			out.ReadyPolling = make(map[string]AWBReadyPollingConfig, len(src.ReadyPolling))
			for key, polling := range src.ReadyPolling {
				polling.Workspace = strings.ToLower(strings.TrimSpace(polling.Workspace))
				polling.Labels = append([]string(nil), polling.Labels...)
				polling.Group = strings.TrimSpace(polling.Group)
				polling.Cwd = strings.TrimSpace(polling.Cwd)
				polling.Interval = strings.TrimSpace(polling.Interval)
				polling.Profile = strings.TrimSpace(polling.Profile)
				polling.SandboxProfile = strings.TrimSpace(polling.SandboxProfile)
				polling.Harness = normalizeHarnessList(polling.Harness)
				polling.Model = strings.TrimSpace(polling.Model)
				polling.Effort = strings.TrimSpace(polling.Effort)
				out.ReadyPolling[strings.ToLower(strings.TrimSpace(key))] = polling
			}
		}
	}
	return out
}

// AWBProxyEnabled reports whether the operator has pointed the daemon at an AWB
// server at all. It is the registration gate — the question "does this host
// have an AWB proxy" — and deliberately NOT the authorization gate: which
// workspaces a caller may reach is agentd.awbEffectiveWorkspaces, which folds the
// allow-list below together with the caller's own grant scope.
//
// It keys on the URL rather than on AllowedWorkspaces because a scope-only
// posture (no operator list, per-agent `awb_workspace` grants) is a supported
// configuration, and an operator running it still has a proxy.
func (c *Config) AWBProxyEnabled() bool {
	return c.ResolvedAWBProxy().URL != ""
}

// AWBWorkspaceAllowed reports whether key names a workspace the operator
// allow-listed. Exact, case-insensitive match on the whole key — there is no
// prefix or wildcard rule here, for the same reason LinearTeamAllowed has none:
// workspace keys are a flat namespace, and a prefix match would let "web"
// authorize "webhooks".
//
// This is the operator half of the gate only. A request is authorized by
// agentd's effective workspace set, which also folds in the caller's grant scope.
func (p AWBProxyConfig) AWBWorkspaceAllowed(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return false
	}
	return slices.Contains(p.AllowedWorkspaces, key)
}

// normalizeGitProxyPatterns trims, lower-cases and de-blanks a pattern list,
// preserving order and dropping duplicates. Lower-casing is safe here because
// every pattern names a DNS host, a forge owner/repo, or a branch — and both
// the remote matcher and the ref matcher compare lower-cased.
// normalizeHarnessList trims each entry, drops the blanks, and de-duplicates
// while preserving the operator's fallback order — the same treatment
// normalizeGitProxyPatterns gives an allow-list, minus the lower-casing, since
// a harness name is matched against the registry verbatim and an unknown one
// is rejected loudly rather than folded.
func normalizeHarnessList(in HarnessList) HarnessList {
	if len(in) == 0 {
		return nil
	}
	out := make(HarnessList, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeGitLabHosts lower-cases the host keys and trims the base URLs,
// dropping entries that are empty or whose base is not https: the daemon
// sends a credential to the base, and a base that is not https would send it
// in clear text, so such an entry is ignored rather than honoured.
func normalizeGitLabHosts(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	// Two entries can normalise to one host (" GitLab.x " and "gitlab.x").
	// With different bases, map order would pick the winner, so neither is
	// trusted: the host is dropped with a warning.
	conflicted := map[string]bool{}
	for rawHost, rawBase := range in {
		host := strings.ToLower(strings.TrimSpace(rawHost))
		base := strings.TrimRight(strings.TrimSpace(rawBase), "/")
		if host == "" || !strings.HasPrefix(strings.ToLower(base), "https://") {
			continue
		}
		// A base is an origin: https://host[:port]. An instance mounted
		// under a path (a GitLab "relative URL root") is refused rather than
		// half-served: its merge request URLs carry the mount in the path,
		// which every merge request, allow-list and project read here would
		// misread as part of the project.
		if u, err := url.Parse(base); err != nil || u.Host == "" || u.Path != "" ||
			u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			slog.Warn("agent.git_proxy.gitlab_hosts: ignoring a base that is not an https origin (no path)",
				"host", host, "base", base)
			continue
		}
		if prev, ok := out[host]; ok && !strings.EqualFold(prev, base) {
			conflicted[host] = true
		}
		out[host] = base
	}
	for host := range conflicted {
		slog.Warn("agent.git_proxy.gitlab_hosts: ignoring a host listed twice with different bases", "host", host)
		delete(out, host)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeGitProxyPatterns(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		p := strings.ToLower(strings.Trim(strings.TrimSpace(raw), "/"))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// HTTPProxyConfig pins a generic proxy to an operator-controlled base URL.
// HeaderValueFile is preferred for secrets; its trimmed contents are appended
// to HeaderValue, which can supply a prefix such as "Bearer ".
type HTTPProxyConfig struct {
	// EnvironmentVariable overrides the default TCLAUDE_HTTP_PROXY_name gateway variable.
	EnvironmentVariable string            `json:"environment_variable,omitempty"`
	URL                 string            `json:"url"`
	Header              string            `json:"header"`
	HeaderValue         string            `json:"header_value,omitempty"`
	HeaderValueFile     string            `json:"header_value_file,omitempty"`
	ModelPolicy         *ModelProxyPolicy `json:"model_policy,omitempty"`
}

// HTTPProxyConfigured reports whether any named HTTP instance is configured.
func (c *Config) HTTPProxyConfigured() bool {
	return c != nil && c.Agent != nil && len(c.Agent.HTTPProxies) > 0
}

// IsHTTPProxyGatewayURL recognizes private loopback capability URLs so a
// renamed or removed custom variable cannot leak into another launch.
func IsHTTPProxyGatewayURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	path := strings.Trim(u.Path, "/")
	if len(path) != 64 {
		return false
	}
	for _, c := range path {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
