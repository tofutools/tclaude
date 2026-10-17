package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// A join code (proto.JoinCode) is what a hub admin hands a node instead of a
// hub URL, an invite and a CA file. Joining with one is the same config
// write as `federation connect`, with the code's CA stored on this node.

type fedJoinReq struct {
	Code    string `json:"code"`
	Name    string `json:"name,omitempty"`
	Preview bool   `json:"preview,omitempty"`
}

// fedJoinResp describes the code and what joining changes. Current is this
// node's connection before the join, so a preview can say what it replaces.
type fedJoinResp struct {
	HubURL    string           `json:"hub_url"`
	Space     string           `json:"space,omitempty"`
	Invite    bool             `json:"invite"`
	ExpiresAt *time.Time       `json:"expires_at,omitempty"`
	Expired   bool             `json:"expired"`
	CA        []proto.CertInfo `json:"ca,omitempty"`
	Current   fedJoinCurrent   `json:"current"`
	Joined    bool             `json:"joined,omitempty"`
	HubCAFile string           `json:"hub_ca_file,omitempty"`
}

type fedJoinCurrent struct {
	HubURL    string `json:"hub_url,omitempty"`
	Enabled   bool   `json:"enabled"`
	Name      string `json:"name"`
	HubCAFile string `json:"hub_ca_file,omitempty"`
}

// handleFederationJoin previews or applies a join code. Applying points this
// node at the code's hub with its invite, trusts the code's CA bundle (or the
// system roots when it has none) and connects.
func handleFederationJoin(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "join a federation hub") {
		return
	}
	var req fedJoinReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*proto.MaxJoinCodeBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	code, err := proto.DecodeJoinCode(req.Code)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_join_code", err.Error())
		return
	}
	if _, err := client.ValidateURL(code.Hub); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_join_code", "the join code's hub address: "+err.Error())
		return
	}
	resp := fedJoinResp{HubURL: code.Hub, Space: code.Space, Invite: code.Invite != "", Current: fedJoinCurrent{Name: defaultFederationName()}}
	if !code.Expires.IsZero() {
		exp := code.Expires
		resp.ExpiresAt = &exp
		resp.Expired = !time.Now().Before(exp)
	}
	if code.CA != "" {
		resp.CA, _ = proto.ParseCABundle(code.CA)
	}
	if cfg, _ := config.Load(); cfg != nil && cfg.Federation != nil {
		f := cfg.Federation
		resp.Current.HubURL, resp.Current.Enabled, resp.Current.HubCAFile = f.HubURL, f.Enabled, f.HubCAFile
		if f.Name != "" {
			resp.Current.Name = f.Name
		}
	}
	if req.Preview {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if resp.Expired {
		writeError(w, http.StatusBadRequest, "join_code_expired", "this join code expired at "+code.Expires.Local().Format(time.DateTime)+"; ask the hub admin for a new one")
		return
	}
	caFile := ""
	if code.CA != "" {
		if caFile, err = storeHubCA(code.CA); err != nil {
			writeError(w, http.StatusInternalServerError, "io", "store the hub CA: "+err.Error())
			return
		}
	}
	on := true
	apply := fedConfigReq{Enabled: &on, HubURL: &code.Hub, Invite: &code.Invite, HubCAFile: &caFile}
	if req.Name != "" {
		apply.Name = &req.Name
	}
	if err := applyFederationConfig(apply); err != nil {
		writeFedErr(w, err)
		return
	}
	resp.Joined, resp.HubCAFile = true, caFile
	writeJSON(w, http.StatusOK, resp)
}

// hubCADir holds CA bundles that join codes brought, under the private data
// directory the daemon reads hub_ca_file from.
func hubCADir() string { return filepath.Join(config.DataDir(), "federation", "hub-ca") }

// storeHubCA writes bundle to a file named after its content, so joining
// again with the same CA reuses the file, and returns the path.
func storeHubCA(bundle string) (string, error) {
	sum := sha256.Sum256([]byte(bundle))
	dir := hubCADir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, hex.EncodeToString(sum[:8])+".pem")
	if existing, err := os.ReadFile(path); err == nil && string(existing) == bundle {
		return path, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".ca-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(bundle); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// hubJoinCode builds a join code for an invite this node minted through its
// hub connection: the hub address and CA file this node uses, which other
// nodes reach the hub with too unless the admin connects through another
// address (then they use the bare invite with the right address). Any error
// leaves the invite usable on its own.
func hubJoinCode(invite, space string, expires time.Time) (string, error) {
	cfg, err := config.Load()
	if err != nil || cfg == nil || cfg.Federation == nil || cfg.Federation.HubURL == "" {
		return "", errors.New("no hub address configured")
	}
	jc := proto.JoinCode{Hub: cfg.Federation.HubURL, Invite: invite, Space: space, Expires: expires}
	if cfg.Federation.HubCAFile != "" {
		raw, err := os.ReadFile(cfg.Federation.HubCAFile)
		if err != nil {
			return "", err
		}
		jc.CA = string(raw)
	}
	return proto.EncodeJoinCode(jc)
}
