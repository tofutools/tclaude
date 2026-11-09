package agentd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
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
	serveNodeUpdateSettings(w, r, "operator")
}

func handleLocalNodeUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage tclaude update settings") {
		return
	}
	serveNodeUpdateSettings(w, r, "operator")
}

func serveNodeUpdateSettings(w http.ResponseWriter, r *http.Request, actor string) {
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
		if errs := next.Validate(); len(errs) > 0 {
			writeJSON(w, 400, map[string]any{"code": "invalid_arg", "error": errs[0], "errors": errs})
			return
		}
		errNeedsConfirm := errors.New("confirm")
		var previous string
		_, err := config.Update(func(cfg *config.Config, loadErr error) error {
			if loadErr != nil {
				return loadErr
			}
			previous = cfg.SelfUpdate().Method
			if selfupdate.IsPackageManagerMethod(next.Method) && next.Method != previous && !req.ConfirmPackageManager {
				return errNeedsConfirm
			}
			if cfg.Updates == nil {
				cfg.Updates = &config.UpdatesConfig{}
			}
			cfg.Updates.Self = &next
			return nil
		})
		if errors.Is(err, errNeedsConfirm) {
			writeError(w, 400, "confirmation_required", "letting tclaude run a package manager is an explicit opt-in: resend with \"confirm_package_manager\": true")
			return
		}
		if err != nil {
			writeError(w, 500, "config_write_failed", err.Error())
			return
		}
		recordFederationAudit("federation.node.update.settings", actor, "", "", "method "+previous+" -> "+next.Method, 200)
	}
	view := buildSelfUpdateSettingsView(r.URL.Query().Get("install_dir"))
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
