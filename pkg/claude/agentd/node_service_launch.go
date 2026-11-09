package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/pflag"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

// The daemon's launch profile (service.launch): the `agentd serve` flags and
// environment tclaude writes into the autostart unit and expects agentd to
// run with. GET suggests a profile from the running daemon ("use what's
// running"); PUT stores one. Secret values never enter config.json:
// secret-looking variables are inherited by name and their value is read
// from the running daemon when the unit is written, and secret-looking flags
// are refused.

// NodeServiceLaunch is the GET/PUT .../service/launch body.
type NodeServiceLaunch struct {
	// Profile is the stored launch profile; null when none is set.
	Profile *config.LaunchProfile `json:"profile"`
	// Version identifies Profile; a PUT that carries a stale one is refused.
	Version string `json:"version"`
	// Suggested is a profile built from the running daemon.
	Suggested launchSuggestion `json:"suggested"`
	// Missing are profile.inherit_env names the running daemon does not
	// have: the unit would be written without them.
	Missing []string `json:"missing"`
	// Owned is false when a unit tclaude did not write runs agentd (or
	// takes the autostart unit's place): the profile is then information
	// only and never applied to it.
	Owned      bool             `json:"owned"`
	Launcher   launchLauncher   `json:"launcher"`
	Comparison launchComparison `json:"comparison"`
	Warnings   []string         `json:"warnings"`
	Access     string           `json:"access"`
	Redacted   []string         `json:"redacted,omitempty"`
}

type launchLauncher struct {
	Kind             string `json:"kind"`
	Unit             string `json:"unit,omitempty"`
	ManagedByTclaude bool   `json:"managed_by_tclaude"`
}

type launchSuggestion struct {
	Flags []launchFlagSuggestion `json:"flags"`
	Env   []launchEnvSuggestion  `json:"env"`
}

// launchFlagSuggestion is one running flag. A secret one carries a redacted
// value and is not accepted into a profile.
type launchFlagSuggestion struct {
	Arg    string `json:"arg"`
	Name   string `json:"name"`
	Secret bool   `json:"secret"`
}

// launchEnvSuggestion is one running variable. Mode is set (an explicit
// value) or inherit (by name; secrets and PATH).
type launchEnvSuggestion struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
	Mode   string `json:"mode"`
}

// launchComparison compares the running daemon with a baseline: the profile
// (profile), the unit tclaude did not write (unit), or none.
type launchComparison struct {
	Baseline    string                        `json:"baseline"`
	State       string                        `json:"state"`
	Differences []selfupdate.LaunchDifference `json:"differences,omitempty"`
}

type launchFieldError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// launchProfileRequest is the PUT body.
type launchProfileRequest struct {
	Profile *config.LaunchProfile `json:"profile"`
	Version string                `json:"version,omitempty"`
}

// relevantEnvRe picks the variables worth suggesting from an interactive
// launch, whose environment is a whole shell's.
var relevantEnvRe = regexp.MustCompile(`^(TCLAUDE_|ANTHROPIC_|CLAUDE_|OPENAI_|CODEX_|OPENCODE_|GEMINI_|GOOGLE_|COPILOT_|GH_|GITHUB_|AWS_|AZURE_|OLLAMA_|XDG_|LC_)|^(LANG|TZ|SSL_CERT_FILE|SSL_CERT_DIR|NODE_EXTRA_CA_CERTS|REQUESTS_CA_BUNDLE)$|^(?i:(https?|no|all)_proxy)$`)

// sessionEnvRe are variables of the login session or of a tclaude pane:
// stale or wrong in a unit started at boot, so never suggested.
var sessionEnvRe = regexp.MustCompile(`^(SSH_AUTH_SOCK|SSH_AGENT_PID|SSH_CONNECTION|SSH_CLIENT|SSH_TTY|DBUS_SESSION_BUS_ADDRESS|XDG_SESSION_.*|XDG_VTNR|XDG_SEAT|GNOME_KEYRING_.*|TMUX.*|WINDOWID|TERM_SESSION_ID|TCLAUDE_SESSION_ID|TCLAUDE_HUMAN_TOKEN|TCLAUDE_AGENTD_SOCKET|TCLAUDE_AGENT_.*|TCLAUDE_CONV_.*)$`)

func launchProfileVersion(p *config.LaunchProfile) string {
	if p == nil {
		return "none"
	}
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func environMap(environ []string) map[string]string {
	out := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			out[k] = v
		}
	}
	return out
}

// serveFlagTokens returns the `serve` flags of argv, one token each in
// --name[=value] form (shorthands and separate values folded in), or
// ok=false when argv is not `agentd serve`.
func serveFlagTokens(argv []string) ([]string, bool) {
	n := -1
	for i, a := range argv {
		if a == "serve" {
			n = i
			break
		}
	}
	if n < 0 || !launcher.IsServeArgv(argv[:n+1]) {
		return nil, false
	}
	return normalizeServeFlags(argv[n+1:])
}

// serveFlagSet returns a fresh `serve` flag set. It is set in init: the
// update path reaches it, and serveCmd reaches the update service, which
// would otherwise be a package initialization cycle.
var serveFlagSet func() *pflag.FlagSet

func init() {
	serveFlagSet = func() *pflag.FlagSet {
		fs := serveCmd().Flags()
		// The root's persistent flag, which `serve` accepts too.
		if fs.Lookup("log-level") == nil {
			fs.String("log-level", "info", "")
		}
		return fs
	}
}

// normalizeServeFlags parses flags with the serve flag set and renders each
// set flag as one --name[=value] token, in argv order.
func normalizeServeFlags(flags []string) ([]string, bool) {
	fs := serveFlagSet()
	fs.SetOutput(io.Discard)
	if fs.Parse(flags) != nil {
		return nil, false
	}
	var order []string
	seen := map[string]bool{}
	for _, tok := range flags {
		if !strings.HasPrefix(tok, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(tok, "-"), "=")
		var f *pflag.Flag
		if strings.HasPrefix(tok, "--") {
			f = fs.Lookup(name)
		} else if name != "" {
			f = fs.ShorthandLookup(name[:1])
		}
		if f != nil && f.Changed && !seen[f.Name] {
			seen[f.Name] = true
			order = append(order, f.Name)
		}
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		f := fs.Lookup(name)
		if f.Value.Type() == "bool" && f.Value.String() == "true" {
			out = append(out, "--"+name)
		} else {
			out = append(out, "--"+name+"="+f.Value.String())
		}
	}
	return out, true
}

// secretServeFlags are the `agentd serve` flags whose value is a secret.
// None today: --persist-operator-token and --print-human-token only say
// where the token goes. A name-based guess would refuse those.
var secretServeFlags = map[string]bool{}

func secretServeFlag(name string) bool { return secretServeFlags[strings.TrimLeft(name, "-")] }

func displayFlag(tok string) string {
	if name, _, ok := strings.Cut(tok, "="); ok && secretServeFlag(name) {
		return name + "=" + launcher.Redacted
	}
	return tok
}

// launchOwned reports whether tclaude may apply a profile: no unit it did
// not write runs agentd or holds the autostart unit's path.
func launchOwned(info launcher.Info, auto launcher.AutostartStatus) bool {
	return (!info.Supervised() || info.ManagedByTclaude) && (!auto.Installed || auto.Managed)
}

func suggestLaunch(info launcher.Info, argv []string, env map[string]string) (launchSuggestion, []string) {
	s := launchSuggestion{Flags: []launchFlagSuggestion{}, Env: []launchEnvSuggestion{}}
	var warnings []string
	if toks, ok := serveFlagTokens(argv); ok {
		for _, tok := range toks {
			name, _, _ := strings.Cut(tok, "=")
			if why, denied := serveFlagsDenied[strings.TrimPrefix(name, "--")]; denied {
				warnings = append(warnings, "Left out "+tok+": "+why+".")
				continue
			}
			s.Flags = append(s.Flags, launchFlagSuggestion{Arg: displayFlag(tok), Name: name, Secret: secretServeFlag(name)})
		}
	} else {
		warnings = append(warnings, "agentd's command line is not `agentd serve`; no flags are suggested.")
	}
	keys := map[string]bool{"PATH": true}
	if info.Supervised() {
		for _, e := range info.Env {
			if !supervisorEnvKeys[e.Key] {
				keys[e.Key] = true
			}
		}
	} else {
		for k := range env {
			if (relevantEnvRe.MatchString(k) || launcher.SecretEnvKey(k)) && !supervisorEnvKeys[k] && !sessionEnvRe.MatchString(k) {
				keys[k] = true
			}
		}
		warnings = append(warnings, "agentd was started from a "+info.Kind+", not a service: only PATH and the variables tclaude and the harnesses use are suggested. Add others yourself.")
	}
	for k := range keys {
		v, ok := env[k]
		if !ok || !launcher.ValidEnvKey(k) {
			continue
		}
		shown := launcher.RedactEnv(k, v)
		secret := shown.Value == launcher.Redacted
		mode := "set"
		if secret || k == "PATH" {
			mode = "inherit"
		}
		s.Env = append(s.Env, launchEnvSuggestion{Key: k, Value: shown.Value, Secret: secret, Mode: mode})
	}
	sort.Slice(s.Env, func(i, j int) bool { return s.Env[i].Key < s.Env[j].Key })
	return s, warnings
}

// compareFlags compares baseline flag tokens with the running ones, by name.
func compareFlags(baseline, running []string) []selfupdate.LaunchDifference {
	index := func(toks []string) map[string]string {
		m := map[string]string{}
		for _, t := range toks {
			name, _, _ := strings.Cut(t, "=")
			m[name] = t
		}
		return m
	}
	b, r := index(baseline), index(running)
	names := []string{}
	for _, m := range []map[string]string{b, r} {
		for k := range m {
			if !slices.Contains(names, k) {
				names = append(names, k)
			}
		}
	}
	sort.Strings(names)
	var out []selfupdate.LaunchDifference
	for _, n := range names {
		bt, inB := b[n]
		rt, inR := r[n]
		switch {
		case !inR:
			out = append(out, selfupdate.LaunchDifference{Kind: "flag", Change: "removed", Name: n, Before: displayFlag(bt)})
		case !inB:
			out = append(out, selfupdate.LaunchDifference{Kind: "flag", Change: "added", Name: n, After: displayFlag(rt)})
		case bt != rt:
			out = append(out, selfupdate.LaunchDifference{Kind: "flag", Change: "changed", Name: n, Before: displayFlag(bt), After: displayFlag(rt)})
		}
	}
	return out
}

// compareEnv checks each baseline variable against the running one. A
// value is only shown when neither side is secret; known=false compares
// presence only.
func compareEnv(baseline []launcher.EnvVar, known func(launcher.EnvVar) bool, env map[string]string) []selfupdate.LaunchDifference {
	var out []selfupdate.LaunchDifference
	for _, e := range baseline {
		v, ok := env[e.Key]
		d := selfupdate.LaunchDifference{Kind: "env", Name: e.Key}
		switch {
		case !ok:
			d.Change = "removed"
		case known(e) && v != e.Value:
			d.Change = "changed"
			if launcher.RedactEnv(e.Key, e.Value).Value != launcher.Redacted && launcher.RedactEnv(e.Key, v).Value != launcher.Redacted {
				d.Before, d.After = e.Value, v
			}
		default:
			continue
		}
		out = append(out, d)
	}
	return out
}

func comparisonOf(baseline string, diffs []selfupdate.LaunchDifference) launchComparison {
	c := launchComparison{Baseline: baseline, State: "identical"}
	if len(diffs) > 0 {
		c.State, c.Differences = "differs", diffs
	}
	return c
}

// compareLaunch compares the running daemon with what it is configured to
// run with: the profile when tclaude owns the launch, else the unit that
// does.
func compareLaunch(profile *config.LaunchProfile, owned bool, info launcher.Info, argv []string, env map[string]string) launchComparison {
	running, ok := serveFlagTokens(argv)
	switch {
	case owned && profile != nil && !info.Supervised():
		// agentd was started by hand; the profile describes the unit
		// started at boot, which is not what runs now.
		return launchComparison{Baseline: "profile", State: "not_applied"}
	case owned && profile != nil:
		if !ok {
			return launchComparison{Baseline: "profile", State: "unavailable"}
		}
		expected, _ := normalizeServeFlags(profile.Flags)
		vars := []launcher.EnvVar{}
		for _, e := range profile.Env {
			vars = append(vars, launcher.EnvVar{Key: e.Key, Value: e.Value})
		}
		inherited := map[string]bool{}
		for _, k := range profile.InheritEnv {
			inherited[k] = true
			vars = append(vars, launcher.EnvVar{Key: k})
		}
		known := func(e launcher.EnvVar) bool { return !inherited[e.Key] }
		return comparisonOf("profile", append(compareFlags(expected, running), compareEnv(vars, known, env)...))
	case !owned && info.Supervised():
		unit, unitOK := serveFlagTokens(info.RawArgv())
		if !ok || !unitOK || info.Wrapped {
			return launchComparison{Baseline: "unit", State: "unavailable"}
		}
		vars := []launcher.EnvVar{}
		for _, e := range info.Env {
			if !supervisorEnvKeys[e.Key] || e.Key == "PATH" {
				vars = append(vars, e)
			}
		}
		known := func(e launcher.EnvVar) bool { return e.Value != launcher.Redacted }
		return comparisonOf("unit", append(compareFlags(unit, running), compareEnv(vars, known, env)...))
	}
	return launchComparison{Baseline: "none", State: "unconfigured"}
}

func nodeServiceLaunchView() NodeServiceLaunch {
	cfg, _ := config.Load()
	profile := cfg.ServiceLaunch()
	info := currentLauncher(false)
	auto := autostartStatus()
	argv, environ := selfUpdateProcess()
	env := environMap(environ)
	owned := launchOwned(info, auto)
	view := NodeServiceLaunch{
		Profile: profile, Version: launchProfileVersion(profile), Missing: []string{}, Owned: owned,
		Launcher:   launchLauncher{Kind: info.Kind, Unit: info.Unit + info.Label, ManagedByTclaude: info.ManagedByTclaude},
		Comparison: compareLaunch(profile, owned, info, argv, env),
	}
	view.Suggested, view.Warnings = suggestLaunch(info, argv, env)
	if view.Warnings == nil {
		view.Warnings = []string{}
	}
	if profile != nil {
		for _, k := range profile.InheritEnv {
			if _, ok := env[k]; !ok {
				view.Missing = append(view.Missing, k)
			}
		}
	}
	if !owned {
		view.Warnings = append(view.Warnings, "A unit tclaude did not write starts agentd; tclaude never rewrites it, so the profile is not applied. The comparison shows how the running daemon differs from that unit.")
	}
	return view
}

// launchViewFor withholds every environment value from a restricted peer.
func launchViewFor(v NodeServiceLaunch, access string) NodeServiceLaunch {
	v.Access = access
	if access == serviceAccessFull {
		return v
	}
	if v.Profile != nil {
		p := *v.Profile
		p.Env = make([]config.LaunchEnv, len(v.Profile.Env))
		for i, e := range v.Profile.Env {
			p.Env[i] = config.LaunchEnv{Key: e.Key}
		}
		v.Profile = &p
	}
	env := make([]launchEnvSuggestion, len(v.Suggested.Env))
	for i, e := range v.Suggested.Env {
		e.Value = ""
		env[i] = e
	}
	v.Suggested.Env = env
	diffs := make([]selfupdate.LaunchDifference, len(v.Comparison.Differences))
	for i, d := range v.Comparison.Differences {
		if d.Kind == "env" {
			d.Before, d.After = "", ""
		}
		diffs[i] = d
	}
	if len(diffs) > 0 {
		v.Comparison.Differences = diffs
	}
	v.Redacted = []string{"profile.env", "suggested.env", "comparison.differences"}
	return v
}

// validateLaunchProfile checks a profile and names each bad field.
func validateLaunchProfile(p *config.LaunchProfile) []launchFieldError {
	var errs []launchFieldError
	add := func(path, code, format string, args ...any) {
		errs = append(errs, launchFieldError{Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	if len(p.Flags) > 48 {
		add("profile.flags", "too_many", "at most 48 serve flags")
	}
	if len(p.Env)+len(p.InheritEnv) > 128 {
		add("profile", "too_many", "at most 128 environment variables")
	}
	for i, f := range p.Flags {
		path := fmt.Sprintf("profile.flags[%d]", i)
		name, value, _ := strings.Cut(f, "=")
		switch {
		case strings.Contains(f, launcher.Redacted):
			add(path, "redacted_value", "%s carries a redacted placeholder, not its value", name)
		case !strings.HasPrefix(f, "--") || name == "--":
			add(path, "invalid_flag", "write each flag as --name or --name=value")
		case secretServeFlag(name):
			add(path, "secret_flag", "%s carries a secret, which the profile never stores; pass it to agentd through an inherited environment variable instead", name)
		default:
			if err := validateServeFlags([]string{f}); err != nil {
				add(path, "invalid_flag", "%s", err.Error())
			} else if strings.ContainsAny(value, "\n\r") {
				add(path, "invalid_flag", "%s contains a line break", name)
			}
		}
	}
	if len(errs) == 0 {
		if err := validateServeFlags(p.Flags); err != nil {
			add("profile.flags", "invalid_flag", "%s", err.Error())
		}
	}
	seen := map[string]string{}
	key := func(path, k string) bool {
		switch {
		case !launcher.ValidEnvKey(k):
			add(path, "invalid_key", "%q is not an environment variable name", k)
		case supervisorEnvKeys[k] && k != "PATH":
			add(path, "reserved_key", "%s is set by the service manager, not the profile", k)
		case seen[k] != "":
			add(path, "duplicate_key", "%s is already in %s", k, seen[k])
		default:
			seen[k] = path
			return true
		}
		return false
	}
	for i, e := range p.Env {
		path := fmt.Sprintf("profile.env[%d]", i)
		if !key(path+".key", e.Key) {
			continue
		}
		switch {
		case strings.Contains(e.Value, launcher.Redacted):
			add(path+".value", "redacted_value", "%s carries a redacted placeholder, not its value", e.Key)
		case launcher.RedactEnv(e.Key, e.Value).Value == launcher.Redacted:
			add(path+".value", "secret_value", "%s looks secret; inherit it from agentd's environment instead of storing its value", e.Key)
		case len(e.Value) > 32*1024 || strings.ContainsFunc(e.Value, func(r rune) bool { return r < 0x20 || r == 0x7f }):
			add(path+".value", "invalid_value", "%s must be one line of at most 32 KiB", e.Key)
		}
	}
	for i, k := range p.InheritEnv {
		key(fmt.Sprintf("profile.inherit_env[%d]", i), k)
	}
	return errs
}

// launchRefusal aborts a profile write with an HTTP answer.
type launchRefusal struct {
	status int
	code   string
	msg    string
	errs   []launchFieldError
}

func (e *launchRefusal) Error() string { return e.msg }

// restrictedLaunchChange lets a peer with node.update.settings change which
// variables an existing profile inherits, nothing else: the flags and the
// explicit values choose what the daemon listens on and trusts.
func restrictedLaunchChange(current, next *config.LaunchProfile) (*config.LaunchProfile, error) {
	refuse := &launchRefusal{http.StatusForbidden, "local_only_setting", "only the node itself or an unrestricted peer may set or clear the launch profile, or change its flags or explicit variables", nil}
	if current == nil || next == nil || !slices.Equal(current.Flags, next.Flags) || len(current.Env) != len(next.Env) {
		return nil, refuse
	}
	for i := range current.Env {
		if current.Env[i].Key != next.Env[i].Key {
			return nil, refuse
		}
	}
	merged := *next
	// The peer saw no values; it keeps the stored ones.
	merged.Env = append([]config.LaunchEnv(nil), current.Env...)
	return &merged, nil
}

func serveNodeServiceLaunch(w http.ResponseWriter, r *http.Request, actor, access string) {
	w.Header().Set("Cache-Control", "private, no-store")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, http.StatusOK, launchViewFor(nodeServiceLaunchView(), access))
		return
	case http.MethodPut:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET to read and PUT to change")
		return
	}
	if access == serviceAccessRead {
		writeError(w, http.StatusForbidden, "permission_denied", "node.update.settings is not shared")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req launchProfileRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "body must be {\"profile\": {...} | null, \"version\": \"...\"}")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_arg", "only one JSON object is allowed")
		return
	}
	next := req.Profile
	if next != nil {
		p := *next
		if p.Flags == nil {
			p.Flags = []string{}
		}
		if p.Env == nil {
			p.Env = []config.LaunchEnv{}
		}
		if p.InheritEnv == nil {
			p.InheritEnv = []string{}
		}
		next = &p
	}
	var previous *config.LaunchProfile
	_, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		previous = cfg.ServiceLaunch()
		if req.Version != "" && req.Version != launchProfileVersion(previous) {
			return &launchRefusal{http.StatusConflict, "launch_profile_conflict", "the launch profile changed since it was read; reload and apply your change again", nil}
		}
		if access != serviceAccessFull {
			merged, err := restrictedLaunchChange(previous, next)
			if err != nil {
				return err
			}
			next = merged
		}
		if next != nil {
			if errs := validateLaunchProfile(next); len(errs) > 0 {
				return &launchRefusal{http.StatusBadRequest, errs[0].Code, errs[0].Message, errs}
			}
		}
		if next == nil {
			if cfg.Service != nil {
				cfg.Service.Launch = nil
				if *cfg.Service == (config.ServiceConfig{}) {
					cfg.Service = nil
				}
			}
			return nil
		}
		if cfg.Service == nil {
			cfg.Service = &config.ServiceConfig{}
		}
		cfg.Service.Launch = next
		return nil
	})
	var refusal *launchRefusal
	if errors.As(err, &refusal) {
		recordFederationAudit("node.service.launch", actor, "", "", "refused: "+refusal.msg, refusal.status)
		if refusal.status == http.StatusConflict {
			body := launchViewFor(nodeServiceLaunchView(), access)
			writeJSON(w, refusal.status, map[string]any{"code": refusal.code, "error": refusal.msg, "current": body})
			return
		}
		body := map[string]any{"code": refusal.code, "error": refusal.msg}
		if refusal.errs != nil {
			body["errors"] = refusal.errs
		}
		writeJSON(w, refusal.status, body)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_write_failed", err.Error())
		return
	}
	recordFederationAudit("node.service.launch", actor, "", "", launchAuditLine(previous, next), http.StatusOK)
	writeJSON(w, http.StatusOK, launchViewFor(nodeServiceLaunchView(), access))
}

// launchAuditLine names what changed, never a value.
func launchAuditLine(before, after *config.LaunchProfile) string {
	if after == nil {
		return "cleared"
	}
	keys := func(p *config.LaunchProfile) (flags, env, inherit []string) {
		if p == nil {
			return
		}
		for _, f := range p.Flags {
			n, _, _ := strings.Cut(f, "=")
			flags = append(flags, n)
		}
		for _, e := range p.Env {
			env = append(env, e.Key)
		}
		return flags, env, p.InheritEnv
	}
	f, e, i := keys(after)
	verb := "set"
	if before != nil {
		verb = "changed"
	}
	return fmt.Sprintf("%s: flags %s; env %s; inherit %s", verb, strings.Join(f, ","), strings.Join(e, ","), strings.Join(i, ","))
}

// profileAutostartSpec is the autostart unit a launch profile describes:
// the running daemon's executable and subcommand, the profile's flags, its
// explicit variables, and the inherited ones read from agentd's environment
// now. PATH is always captured.
func profileAutostartSpec(info launcher.Info, p *config.LaunchProfile, ownEnvFile string) (launcher.AutostartSpec, []string, error) {
	exe, _, err := splitServeArgv(info.RawArgv())
	if err != nil {
		return launcher.AutostartSpec{}, nil, err
	}
	if errs := validateLaunchProfile(p); len(errs) > 0 {
		return launcher.AutostartSpec{}, nil, fmt.Errorf("the launch profile is invalid at %s: %s", errs[0].Path, errs[0].Message)
	}
	var warnings []string
	// The environment agentd started with, as the view's suggestion and
	// "missing" read it, not what serve exported into it since.
	_, environ := selfUpdateProcess()
	env := environMap(environ)
	for _, f := range info.EnvironmentFiles {
		if f != ownEnvFile {
			warnings = append(warnings, "The launch profile replaces the observed launch, so the unit no longer reads "+f+"; inherit the variables you need from it.")
		}
	}
	spec := launcher.AutostartSpec{Argv: append(exe, p.Flags...)}
	have := map[string]bool{}
	for _, e := range p.Env {
		spec.Env = append(spec.Env, launcher.EnvVar{Key: e.Key, Value: e.Value})
		have[e.Key] = true
	}
	inherit := append([]string(nil), p.InheritEnv...)
	if !have["PATH"] && !slices.Contains(inherit, "PATH") {
		inherit = append(inherit, "PATH")
	}
	for _, k := range inherit {
		if v, ok := env[k]; ok {
			spec.Env = append(spec.Env, launcher.EnvVar{Key: k, Value: v})
		} else {
			warnings = append(warnings, k+" is not set in agentd's environment, so the unit does not set it.")
		}
	}
	if info.Supervised() && !info.ManagedByTclaude {
		warnings = append(warnings, "agentd runs under "+info.Kind+" "+info.Unit+info.Label+", which tclaude did not write and never rewrites. Disable it, or both start at boot and the second one stops at once.")
	}
	return spec, warnings, launcher.ValidateSpec(spec)
}

// errLaunchProfileSet refuses autostart options a launch profile decides.
var errLaunchProfileSet = errors.New("the launch profile decides the unit's flags and environment; edit it (tclaude service launch set) instead of passing serve_args or keep_env")

// expectedLaunchAfterUpdate is the restarted daemon against its configured
// launch, for the update job; nil when there is none to compare with.
func expectedLaunchAfterUpdate() *selfupdate.ExpectedLaunch {
	cfg, _ := config.Load()
	info := currentLauncher(false)
	argv, environ := selfUpdateProcess()
	c := compareLaunch(cfg.ServiceLaunch(), launchOwned(info, autostartStatus()), info, argv, environMap(environ))
	if c.State != "identical" && c.State != "differs" {
		return nil
	}
	return &selfupdate.ExpectedLaunch{Baseline: c.Baseline, State: c.State, Differences: c.Differences}
}
