package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
)

// Remote-access cert management (JOH-278). These endpoints generate / read /
// hand out the remote-access secret material (CA, client .p12s, the login
// passphrase) for the Config tab. They are served over BOTH the loopback
// dashboard and the remote (mTLS + passphrase) listener: a remote session has
// already cleared a client certificate AND the passphrase and is already a full
// control-plane operator (it can spawn/kill agents and inject keystrokes), so
// cert management is consistent with that privilege level — no separate network
// boundary is warranted. Auth is the standard dashboard gate (loopback cookie or
// the remote listener's pre-auth tag).

// requireCertAdmin gates a cert-management handler on the standard dashboard
// auth (loopback session cookie, or a request the remote listener already
// authenticated). Writes the error response and returns false when refused.
func requireCertAdmin(w http.ResponseWriter, r *http.Request) bool {
	return checkDashboardAuth(w, r)
}

// decodeJSONCertAdmin decodes a (size-capped) JSON request body, writing a 400
// and returning false on failure.
func decodeJSONCertAdmin(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func registerRemoteAccessAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/remote-access/info", handleRemoteAccessInfo)
	mux.HandleFunc("/api/remote-access/ca.crt", handleRemoteAccessCADownload)
	mux.HandleFunc("/api/remote-access/client", handleRemoteAccessClientDownload)
	mux.HandleFunc("/api/remote-access/add-client", handleRemoteAccessAddClient)
	mux.HandleFunc("/api/remote-access/add-hosts", handleRemoteAccessAddHosts)
	mux.HandleFunc("/api/remote-access/setup", handleRemoteAccessSetup)
}

// remoteAccessInfo is the cert/admin view for the Config tab: enough to render
// device management without putting cert reads on the 2s snapshot.
type remoteAccessInfo struct {
	MaterialExists bool                     `json:"material_exists"`
	Running        bool                     `json:"running"`
	RunningBind    string                   `json:"running_bind"`
	Enabled        bool                     `json:"enabled"`
	Bind           string                   `json:"bind"`
	CAPresent      bool                     `json:"ca_present"`
	SANs           []string                 `json:"sans"`
	Clients        []remoteaccess.ClientInfo `json:"clients"`
}

func handleRemoteAccessInfo(w http.ResponseWriter, r *http.Request) {
	if !requireCertAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	cfg, _ := config.Load()
	running, bind := remoteListenerStatus()
	info := remoteAccessInfo{
		MaterialExists: remoteaccess.Exists(),
		Running:        running,
		RunningBind:    bind,
		Enabled:        cfg.RemoteAccessEnabled(),
		Bind:           cfg.RemoteAccessBind(),
		SANs:           []string{},
		Clients:        []remoteaccess.ClientInfo{},
	}
	if info.MaterialExists {
		info.CAPresent = true
		if sans, err := remoteaccess.ServerCertSANs(); err == nil {
			info.SANs = sans
		} else {
			slog.Warn("remote-access info: read server cert SANs", "err", err)
		}
		if clients, err := remoteaccess.ListClients(); err == nil && clients != nil {
			info.Clients = clients
		} else if err != nil {
			slog.Warn("remote-access info: list clients", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, info)
}

func handleRemoteAccessCADownload(w http.ResponseWriter, r *http.Request) {
	if !requireCertAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	data, err := remoteaccess.CACert()
	if err != nil {
		http.Error(w, "no CA certificate — run setup first", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="tclaude-remote-access-ca.crt"`)
	_, _ = w.Write(data)
}

func handleRemoteAccessClientDownload(w http.ResponseWriter, r *http.Request) {
	if !requireCertAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if !remoteaccess.ValidClientName(name) {
		http.Error(w, "invalid client name", http.StatusBadRequest)
		return
	}
	data, err := remoteaccess.ClientP12(name)
	if err != nil {
		http.Error(w, "no .p12 for that device", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-pkcs12")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, name+".p12"))
	_, _ = w.Write(data)
}

// addClientRequest is the add-device form body. The .p12 password protects the
// private key in the downloaded bundle and is never stored server-side.
type addClientRequest struct {
	Name        string `json:"name"`
	P12Password string `json:"p12_password"`
}

func handleRemoteAccessAddClient(w http.ResponseWriter, r *http.Request) {
	if !requireCertAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req addClientRequest
	if !decodeJSONCertAdmin(w, r, &req) {
		return
	}
	res, err := remoteaccess.AddClient(strings.TrimSpace(req.Name), req.P12Password, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": res.Name})
}

// addHostsRequest carries extra host names / IPs to add to the server cert's
// SANs (a public URL, a tailnet name) so a new address verifies cleanly.
type addHostsRequest struct {
	Hosts string `json:"hosts"` // comma/space/newline-separated
}

// handleRemoteAccessAddHosts reissues the SERVER cert (under the existing CA)
// with additional SANs. Non-destructive: installed client devices keep working
// because the CA is unchanged. The running listener serves the new cert after an
// agentd restart.
func handleRemoteAccessAddHosts(w http.ResponseWriter, r *http.Request) {
	if !requireCertAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req addHostsRequest
	if !decodeJSONCertAdmin(w, r, &req) {
		return
	}
	hosts := splitSANHosts(req.Hosts)
	if len(hosts) == 0 {
		http.Error(w, "no host names given", http.StatusBadRequest)
		return
	}
	cfg, _ := config.Load()
	sans, err := remoteaccess.ReissueServerCert(cfg.RemoteAccessBind(), hosts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sans": sans})
}

// setupRequest is the first-time-setup / regenerate form body. Passphrase and
// p12_password are accepted over the authenticated dashboard connection and
// never logged.
type setupRequest struct {
	Bind        string `json:"bind"`
	Hosts       string `json:"hosts"` // comma/space-separated extra SANs
	Passphrase  string `json:"passphrase"`
	P12Password string `json:"p12_password"`
	ClientName  string `json:"client_name"`
	Regenerate  bool   `json:"regenerate"`
	Enable      bool   `json:"enable"`
}

func handleRemoteAccessSetup(w http.ResponseWriter, r *http.Request) {
	if !requireCertAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req setupRequest
	if !decodeJSONCertAdmin(w, r, &req) {
		return
	}
	req.Bind = strings.TrimSpace(req.Bind)
	if req.Bind == "" {
		http.Error(w, "a bind address is required (e.g. 0.0.0.0:8443)", http.StatusBadRequest)
		return
	}
	res, err := remoteaccess.Setup(remoteaccess.SetupOptions{
		Bind:            req.Bind,
		ExtraHosts:      splitSANHosts(req.Hosts),
		Passphrase:      req.Passphrase,
		ClientName:      strings.TrimSpace(req.ClientName),
		P12Password:     req.P12Password,
		RegenerateCerts: req.Regenerate,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Optionally flip remote_access on in config.json (the listener still needs
	// an agentd restart to actually start — same as the CLI's --enable).
	if req.Enable {
		if _, err := config.Update(func(cfg *config.Config, _ error) error {
			cfg.RemoteAccess = &config.RemoteAccessConfig{Enabled: true, Bind: req.Bind}
			return nil
		}); err != nil {
			http.Error(w, "material generated but enabling in config.json failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"client_name": res.ClientName,
		"hosts":       res.Hosts,
		"enabled":     req.Enable,
	})
}

// splitSANHosts splits a comma/space/newline-separated host list into trimmed,
// non-empty entries.
func splitSANHosts(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
