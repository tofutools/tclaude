package agentd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

// selfUpdateSettingsView is GET/PUT /api/node/update/settings: the effective
// updates.self plus what the method picker needs to explain the choice.
type selfUpdateSettingsView struct {
	Settings         config.SelfUpdateConfig `json:"settings"`
	MethodChosen     bool                    `json:"method_chosen"`
	Detected         *selfupdate.Detection   `json:"detected,omitempty"`
	DetectionError   string                  `json:"detection_error,omitempty"`
	Methods          []selfupdate.MethodInfo `json:"methods"`
	SetupFlags       []string                `json:"setup_flags"`
	SetupHarnesses   []string                `json:"setup_harnesses"`
	InstallDir       string                  `json:"install_dir"`
	InstallShadowed  bool                    `json:"install_dir_shadowed"`
	ShadowedBy       string                  `json:"shadowed_by,omitempty"`
	Warnings         []string                `json:"warnings"`
	NextScheduledRun *time.Time              `json:"next_scheduled_run"`
	LastScheduledRun *selfUpdateScheduledRun `json:"last_scheduled_run"`
	// Redacted lists settings withheld from a restricted peer; sending them
	// back empty leaves them unchanged.
	Redacted []string `json:"redacted,omitempty"`
}

// selfUpdateSettingsRequest is the PUT body. Switching to a package-manager
// method is an explicit opt-in and needs ConfirmPackageManager.
type selfUpdateSettingsRequest struct {
	Settings              *config.SelfUpdateConfig `json:"settings"`
	ConfirmPackageManager bool                     `json:"confirm_package_manager,omitempty"`
}

func registerNodeUpdateSettingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/node/update/settings", handleDashboardNodeUpdateSettings)
	mux.HandleFunc("PUT /api/node/update/settings", handleDashboardNodeUpdateSettings)
}

func handleDashboardNodeUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	serveNodeUpdateSettings(w, r, "operator", false)
}

func handleLocalNodeUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage tclaude update settings") {
		return
	}
	serveNodeUpdateSettings(w, r, "operator", false)
}

// servePeerNodeUpdateSettings serves a peer: reading needs node.update and
// changing needs node.update.settings, both re-checked here. Only an
// unrestricted peer acts as this node's own operator; any other peer gets
// the restricted view and changes (see restrictedSettingsChange).
func servePeerNodeUpdateSettings(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !peerNodeUpdateAllowed(v, rule) {
		writeError(w, 403, "permission_denied", rule.requires+" is not shared")
		return
	}
	serveNodeUpdateSettings(w, r, "operator@"+v.peer.InstanceID, !v.unrestricted())
}

// settingsRefusal aborts a config.Update with an HTTP answer.
type settingsRefusal struct {
	status int
	code   string
	msg    string
	errs   []string
}

func (e *settingsRefusal) Error() string { return e.msg }

// restrictedSettingsChange applies a restricted peer's change on top of the
// node's current settings. Free operator text (the custom command, the
// checkout path), the install dir and the package-manager acknowledgement
// stay local, and a restricted peer can never opt the node into running a
// package manager or into a free-text method. Switching back to manual is
// always allowed.
func restrictedSettingsChange(current, next config.SelfUpdateConfig) (config.SelfUpdateConfig, error) {
	current, next = current.Resolved(), next.Resolved()
	if next.Custom.Command == "" {
		next.Custom.Command = current.Custom.Command // the peer saw it redacted
	}
	localOnly := map[string]bool{
		"custom.command":       next.Custom.Command != current.Custom.Command,
		"source_checkout.path": next.SourceCheckout.Path != current.SourceCheckout.Path,
		"install_dir":          next.InstallDir != current.InstallDir,
		"package_manager_ack":  next.PackageManagerAck != current.PackageManagerAck,
	}
	for _, field := range []string{"custom.command", "source_checkout.path", "install_dir", "package_manager_ack"} {
		if localOnly[field] {
			return next, &settingsRefusal{403, "local_only_setting", "updates.self." + field + " can only be changed on the node itself or by an unrestricted peer", nil}
		}
	}
	if next.Method != current.Method && (selfupdate.IsPackageManagerMethod(next.Method) || selfupdate.IsFreeTextMethod(next.Method)) {
		return next, &settingsRefusal{403, "local_only_setting", "switching to the " + next.Method + " method needs the node's own operator or an unrestricted peer", nil}
	}
	return next, nil
}

// changedSelfUpdateFields names the top-level updates.self keys a change
// touched, for the audit trail.
func changedSelfUpdateFields(before, after config.SelfUpdateConfig) string {
	var a, b map[string]json.RawMessage
	rawA, _ := json.Marshal(before.Resolved())
	rawB, _ := json.Marshal(after.Resolved())
	_ = json.Unmarshal(rawA, &a)
	_ = json.Unmarshal(rawB, &b)
	keys := []string{}
	for k := range b {
		if string(a[k]) != string(b[k]) {
			keys = append(keys, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := "changed: " + strings.Join(keys, ",")
	if before.Method != after.Method {
		out += " method " + before.Method + " -> " + after.Method
	}
	return out
}

func serveNodeUpdateSettings(w http.ResponseWriter, r *http.Request, actor string, restricted bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Method == http.MethodPut {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var req selfUpdateSettingsRequest
		if err := dec.Decode(&req); err != nil || req.Settings == nil {
			writeError(w, 400, "invalid_arg", "body must be {\"settings\": {...}}")
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			writeError(w, 400, "invalid_arg", "only one settings JSON object is allowed")
			return
		}
		next := *req.Settings
		var previous config.SelfUpdateConfig
		_, err := config.Update(func(cfg *config.Config, loadErr error) error {
			if loadErr != nil {
				return loadErr
			}
			previous = cfg.SelfUpdate()
			if restricted {
				merged, err := restrictedSettingsChange(previous, next)
				if err != nil {
					return err
				}
				next = merged
			}
			if errs := next.Validate(); len(errs) > 0 {
				return &settingsRefusal{400, "invalid_arg", errs[0], errs}
			}
			if selfupdate.IsPackageManagerMethod(next.Method) && next.Method != previous.Method && !req.ConfirmPackageManager {
				return &settingsRefusal{400, "confirmation_required", "letting tclaude run a package manager is an explicit opt-in: resend with \"confirm_package_manager\": true", nil}
			}
			if cfg.Updates == nil {
				cfg.Updates = &config.UpdatesConfig{}
			}
			cfg.Updates.Self = &next
			return nil
		})
		var refusal *settingsRefusal
		if errors.As(err, &refusal) {
			body := map[string]any{"code": refusal.code, "error": refusal.msg}
			if refusal.errs != nil {
				body["errors"] = refusal.errs
			}
			recordFederationAudit("federation.node.update.settings", actor, "", "", "refused: "+refusal.msg, refusal.status)
			writeJSON(w, refusal.status, body)
			return
		}
		if err != nil {
			writeError(w, 500, "config_write_failed", err.Error())
			return
		}
		recordFederationAudit("federation.node.update.settings", actor, "", "", changedSelfUpdateFields(previous, next), 200)
	}
	view := buildSelfUpdateSettingsView(r.URL.Query().Get("install_dir"))
	if restricted && view.Settings.Custom != nil && view.Settings.Custom.Command != "" {
		// The command is the operator's free text and may carry secrets.
		view.Settings.Custom.Command = ""
		view.Redacted = append(view.Redacted, "custom.command")
	}
	writeJSON(w, 200, view)
}

func buildSelfUpdateSettingsView(installDirOverride string) selfUpdateSettingsView {
	cfg, _ := config.Load()
	settings := cfg.SelfUpdate()
	view := selfUpdateSettingsView{
		Settings:       settings,
		MethodChosen:   settings.Method != "",
		SetupFlags:     config.SetupFlags,
		SetupHarnesses: config.SetupHarnesses,
		Warnings:       []string{},
		Methods:        []selfupdate.MethodInfo{},
	}
	var bins []selfupdate.Binary
	if svc, err := localUpdateService(); err != nil {
		view.DetectionError = err.Error()
	} else {
		bins = svc.Status().Binaries
		d := selfupdate.Detect(bins)
		view.Detected = &d
		view.Methods = selfupdate.Methods(bins)
	}
	home, _ := canonicalOperatorHome()
	dir := settings.InstallDir
	if installDirOverride != "" {
		dir = installDirOverride
	}
	view.InstallDir = config.ExpandHome(dir, home)
	managed := false
	for _, b := range bins {
		if b.ManagedBy == "" {
			continue
		}
		managed = true
		if filepath.IsAbs(view.InstallDir) {
			if w := selfupdate.ShadowWarning(b.Name, filepath.Join(view.InstallDir, b.Name)); w != "" && !view.InstallShadowed {
				view.InstallShadowed = true
				view.ShadowedBy = b.Path
			}
		}
		if selfupdate.TclaudeDriven(settings.Method) {
			view.Warnings = append(view.Warnings, (&selfupdate.PackageManagedError{Binary: b, NeedsAck: true}).Error())
		}
	}
	if !managed {
		// Shadowing only matters next to a package-managed binary.
		view.InstallShadowed, view.ShadowedBy = false, ""
	}
	if view.Detected != nil && settings.Method != "" && settings.Method != selfupdate.MethodManual && view.Detected.Method != selfupdate.MethodManual && settings.Method != view.Detected.Method {
		view.Warnings = append(view.Warnings, "tclaude was installed with "+view.Detected.Method+"; updating with "+settings.Method+" may be shadowed or overwritten by the other tool.")
	}
	state := loadSelfUpdateScheduleState()
	view.LastScheduledRun = state.LastRun
	if next := settings.Schedule.NextRun(selfUpdateNow(), state.LastSlot); settings.Method != "" && !next.IsZero() {
		view.NextScheduledRun = &next
	}
	return view
}

// selfUpdateApplyOptions turns the node's own updates.self into apply
// options. Nothing here comes from a request.
func selfUpdateApplyOptions(s config.SelfUpdateConfig) selfupdate.ApplyOptions {
	home, _ := canonicalOperatorHome()
	opts := selfupdate.ApplyOptions{
		Method:            s.Method,
		Checkout:          selfupdate.CheckoutOptions{Path: config.ExpandHome(s.SourceCheckout.Path, home), Ref: s.SourceCheckout.Ref},
		CustomCommand:     s.Custom.Command,
		CustomTimeout:     time.Duration(s.Custom.TimeoutSeconds) * time.Second,
		PackageManagerAck: s.PackageManagerAck,
		InstallDir:        config.ExpandHome(s.InstallDir, home),
	}
	if s.PostUpdateSetup.Enabled {
		opts.PostSetup = s.PostUpdateSetup.SetupArgs()
	}
	return opts
}
