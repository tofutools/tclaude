package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"golang.org/x/net/http/httpguts"
)

// Gateway setup lets the operator create, edit, test and remove a model
// gateway from the dashboard instead of hand-editing agent.http_proxies. A
// pasted provider key is written once to a 0600 file under the private data
// directory and referenced by header_value_file; no read returns it.

// modelGatewaySetup is both the editable view of one gateway and the body
// that saves or tests it. Key is write-only.
type modelGatewaySetup struct {
	URL          string `json:"url"`
	Header       string `json:"header"`
	HeaderPrefix string `json:"header_prefix,omitempty"`
	// Key is a pasted provider credential; KeyFile names an operator-managed
	// file instead. Neither keeps the stored credential.
	Key     string `json:"key,omitempty"`
	KeyFile string `json:"key_file,omitempty"`
	// Credential says where the stored credential lives: managed (a key
	// pasted here), file (KeyFile), inline (header_value in config) or none.
	Credential string                  `json:"credential,omitempty"`
	Policy     config.ModelProxyPolicy `json:"policy"`
	Create     bool                    `json:"create,omitempty"`
	Name       string                  `json:"name,omitempty"`
}

func modelGatewayKeyDir() string { return filepath.Join(config.DataDir(), "model-gateways") }

func modelGatewayManagedKey(path string) bool {
	return path != "" && filepath.Dir(path) == modelGatewayKeyDir()
}

func modelGatewayKeyFileInDataDir(path string) bool {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return true
		}
		path = filepath.Join(home, path[2:])
	}
	data := filepath.Clean(config.DataDir())
	if resolved, err := filepath.EvalSymlinks(data); err == nil {
		data = resolved
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path == data || strings.HasPrefix(path, data+string(filepath.Separator))
}

func modelGatewayCredentialSource(instance config.HTTPProxyConfig) string {
	switch {
	case modelGatewayManagedKey(instance.HeaderValueFile):
		return "managed"
	case instance.HeaderValueFile != "":
		return "file"
	case instance.HeaderValue != "":
		return "inline"
	}
	return "none"
}

func modelGatewayView(name string, instance config.HTTPProxyConfig) modelGatewaySetup {
	out := modelGatewaySetup{Name: name, URL: instance.URL, Header: instance.Header, Credential: modelGatewayCredentialSource(instance)}
	if instance.HeaderValueFile != "" {
		// Only with a file is header_value a prefix; inline it is the secret.
		out.HeaderPrefix = instance.HeaderValue
		if out.Credential == "file" {
			out.KeyFile = instance.HeaderValueFile
		}
	} else if strings.HasPrefix(instance.HeaderValue, "Bearer ") {
		// An inline secret's scheme is not secret; a key pasted to replace
		// it keeps the same scheme.
		out.HeaderPrefix = "Bearer "
	}
	if instance.ModelPolicy != nil {
		out.Policy = *instance.ModelPolicy
	}
	return out
}

func decodeModelGatewaySetup(w http.ResponseWriter, r *http.Request) (modelGatewaySetup, bool) {
	var in modelGatewaySetup
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil {
		modelError(w, 400, "invalid model gateway setup")
		return in, false
	}
	in.URL = strings.TrimSpace(in.URL)
	in.Header = strings.TrimSpace(in.Header)
	in.Key = strings.TrimSpace(in.Key)
	in.KeyFile = strings.TrimSpace(in.KeyFile)
	if in.Policy.Dialect == "anthropic" {
		in.Policy.Dialect = ""
	}
	return in, true
}

// credentialFields resolves the credential half of a gateway: the new key or
// key file when given, else the stored one (nil when there is none).
func (in modelGatewaySetup) credentialFields(stored *config.HTTPProxyConfig, keyPath string) (value, file string, err error) {
	if !httpguts.ValidHeaderFieldValue(in.HeaderPrefix) {
		return "", "", errors.New("the header prefix contains characters a header cannot carry")
	}
	switch {
	case in.Key != "":
		if len(in.Key) > 16384 || !httpguts.ValidHeaderFieldValue(in.HeaderPrefix+in.Key) {
			return "", "", errors.New("the API key is too long or contains characters a header cannot carry")
		}
		if keyPath == "" {
			return in.HeaderPrefix + in.Key, "", nil
		}
		return in.HeaderPrefix, keyPath, nil
	case in.KeyFile != "":
		if !filepath.IsAbs(in.KeyFile) && !strings.HasPrefix(in.KeyFile, "~/") {
			return "", "", errors.New("the key file must be an absolute path or start with ~/")
		}
		if modelGatewayKeyFileInDataDir(in.KeyFile) {
			// Pasted keys and daemon secrets live there; deleting a gateway
			// removes its pasted key, so sharing one would break another.
			return "", "", errors.New("the key file cannot be inside tclaude's data directory; paste the key instead")
		}
		if _, err := httpProxyCredential(config.HTTPProxyConfig{HeaderValue: in.HeaderPrefix, HeaderValueFile: in.KeyFile}); err != nil {
			return "", "", fmt.Errorf("the key file is unreadable, empty or invalid")
		}
		return in.HeaderPrefix, in.KeyFile, nil
	case stored == nil || modelGatewayCredentialSource(*stored) == "none":
		return "", "", errors.New("paste the provider API key or name a file that holds it")
	case stored.HeaderValueFile == "":
		return stored.HeaderValue, "", nil
	}
	return in.HeaderPrefix, stored.HeaderValueFile, nil
}

func (in modelGatewaySetup) validate() error {
	if _, err := httpProxyURL(in.URL, ""); err != nil {
		return errors.New("the provider URL must be http(s)://host[/prefix], without credentials, query or fragment")
	}
	if !httpProxyHeaderAllowed(in.Header) {
		return errors.New("name the header that carries the key, such as x-api-key or Authorization")
	}
	return modelPolicyProblem(&in.Policy)
}

// writeModelGatewayKey stores a pasted key in a fresh 0600 file, so a failed
// save never leaves the old file half-replaced.
func writeModelGatewayKey(name, key string) (string, error) {
	dir := modelGatewayKeyDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, name+"-*.key")
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(key + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(f.Name(), 0o600)
	}
	if werr != nil {
		_ = os.Remove(f.Name())
		return "", werr
	}
	return f.Name(), nil
}

func handleModelGatewayGet(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read model gateway setup") {
		return
	}
	name := r.PathValue("name")
	cfg, err := config.Load()
	if err != nil {
		modelError(w, 503, "model gateway configuration unavailable")
		return
	}
	var instance config.HTTPProxyConfig
	ok := false
	if cfg.Agent != nil {
		instance, ok = cfg.Agent.HTTPProxies[name]
	}
	if !ok || instance.ModelPolicy == nil {
		modelError(w, 404, "no model gateway by that name")
		return
	}
	writeJSON(w, 200, modelGatewayView(name, instance))
}

func handleModelGatewayPut(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "set up model gateways") {
		return
	}
	name := r.PathValue("name")
	if !validModelProxyName(name) {
		modelError(w, 400, "the gateway name is 1-64 letters, digits, . _ -")
		return
	}
	in, ok := decodeModelGatewaySetup(w, r)
	if !ok {
		return
	}
	if err := in.validate(); err != nil {
		modelError(w, 400, err.Error())
		return
	}
	// Turning a gateway off revokes its leases first and fails closed, like
	// the switch: a lease must never outlive an off it was told about.
	if cfg, err := config.Load(); err == nil && cfg.Agent != nil && !in.Policy.Enabled {
		if stored, ok := cfg.Agent.HTTPProxies[name]; ok && stored.ModelPolicy != nil && stored.ModelPolicy.Enabled && !in.Create {
			if err := db.RevokeModelProxyLeaseSelection(name, ""); err != nil {
				modelError(w, 503, "model gateway lease revocation unavailable")
				return
			}
		}
	}
	newKey := ""
	if in.Key != "" {
		// Check the key before it reaches disk.
		if _, _, err := in.credentialFields(nil, ""); err != nil {
			modelError(w, 400, err.Error())
			return
		}
		path, err := writeModelGatewayKey(name, in.Key)
		if err != nil {
			modelError(w, 503, "could not store the API key")
			return
		}
		newKey = path
	}
	oldFile, savedFile, wasEnabled := "", "", false
	_, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		if cfg.Agent == nil {
			cfg.Agent = &config.AgentConfig{}
		}
		if cfg.Agent.HTTPProxies == nil {
			cfg.Agent.HTTPProxies = map[string]config.HTTPProxyConfig{}
		}
		stored, exists := cfg.Agent.HTTPProxies[name]
		if in.Create && exists {
			return fmt.Errorf("an HTTP proxy named %s already exists; pick another name", name)
		}
		if !in.Create && (!exists || stored.ModelPolicy == nil) {
			return errors.New("no model gateway by that name")
		}
		var prior *config.HTTPProxyConfig
		if exists {
			prior = &stored
			wasEnabled = stored.ModelPolicy.Enabled
		}
		value, file, err := in.credentialFields(prior, newKey)
		if err != nil {
			return err
		}
		policy := in.Policy
		if prior != nil {
			// Blocks change through the switch; never from a stale form.
			policy.BlockedPeers = stored.ModelPolicy.BlockedPeers
		}
		oldFile, savedFile = stored.HeaderValueFile, file
		stored.URL, stored.Header, stored.HeaderValue, stored.HeaderValueFile = in.URL, in.Header, value, file
		stored.ModelPolicy = &policy
		cfg.Agent.HTTPProxies[name] = stored
		return nil
	})
	if err != nil {
		if newKey != "" {
			_ = os.Remove(newKey)
		}
		modelError(w, 400, err.Error())
		return
	}
	if oldFile != savedFile && modelGatewayManagedKey(oldFile) {
		_ = os.Remove(oldFile)
	}
	if wasEnabled && !in.Policy.Enabled {
		_ = db.RevokeModelProxyLeaseSelection(name, "")
	}
	cfg, err := config.Load()
	if err != nil || cfg.Agent == nil {
		modelError(w, 503, "model gateway configuration unavailable")
		return
	}
	writeJSON(w, 200, modelGatewayView(name, cfg.Agent.HTTPProxies[name]))
}

func handleModelGatewayDelete(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "remove model gateways") {
		return
	}
	name := r.PathValue("name")
	if err := db.RevokeModelProxyLeaseSelection(name, ""); err != nil {
		modelError(w, 503, "model gateway lease revocation unavailable")
		return
	}
	oldFile := ""
	_, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		if cfg.Agent == nil || cfg.Agent.HTTPProxies[name].ModelPolicy == nil {
			return errors.New("no model gateway by that name")
		}
		oldFile = cfg.Agent.HTTPProxies[name].HeaderValueFile
		delete(cfg.Agent.HTTPProxies, name)
		return nil
	})
	if err != nil {
		modelError(w, 404, err.Error())
		return
	}
	if modelGatewayManagedKey(oldFile) {
		_ = os.Remove(oldFile)
	}
	writeJSON(w, 200, map[string]any{"name": name, "removed": true})
}

type modelGatewayTestResult struct {
	OK      bool     `json:"ok"`
	Status  int      `json:"status,omitempty"`
	Message string   `json:"message"`
	Models  []string `json:"models,omitempty"`
}

var modelGatewayTestClient = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}

// handleModelGatewayTest lists the provider's models with the draft's URL and
// key (or the stored key of the named gateway). It spends no tokens. Provider
// bodies other than the model list are never echoed: they can reflect the key.
func handleModelGatewayTest(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "test model gateways") {
		return
	}
	in, ok := decodeModelGatewaySetup(w, r)
	if !ok {
		return
	}
	if _, err := httpProxyURL(in.URL, ""); err != nil || !httpProxyHeaderAllowed(in.Header) {
		writeJSON(w, 200, modelGatewayTestResult{Message: "Give the provider URL and the header that carries the key."})
		return
	}
	var stored *config.HTTPProxyConfig
	if in.Key == "" && in.KeyFile == "" {
		if cfg, err := config.Load(); err == nil && cfg.Agent != nil {
			if instance, ok := cfg.Agent.HTTPProxies[in.Name]; ok && instance.ModelPolicy != nil {
				stored = &instance
			}
		}
	}
	value, file, err := in.credentialFields(stored, "")
	if err != nil {
		writeJSON(w, 200, modelGatewayTestResult{Message: err.Error()})
		return
	}
	credential, err := httpProxyCredential(config.HTTPProxyConfig{HeaderValue: value, HeaderValueFile: file})
	if err != nil {
		writeJSON(w, 200, modelGatewayTestResult{Message: "The stored key is unreadable or invalid."})
		return
	}
	path := "/v1/models"
	if in.Policy.Dialect != "openai" {
		path += "?limit=1000"
	}
	target, _ := httpProxyURL(in.URL, path)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	req.Header.Set(in.Header, credential)
	if in.Policy.Dialect != "openai" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	resp, err := modelGatewayTestClient.Do(req)
	if err != nil {
		writeJSON(w, 200, modelGatewayTestResult{Message: "Could not reach the provider URL."})
		return
	}
	defer resp.Body.Close()
	res := modelGatewayTestResult{Status: resp.StatusCode}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		res.Message = fmt.Sprintf("The provider refused the key (HTTP %d).", resp.StatusCode)
	case resp.StatusCode != 200:
		res.Message = fmt.Sprintf("The provider answered HTTP %d to a model list request; check the URL.", resp.StatusCode)
	default:
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if rerr != nil || json.Unmarshal(body, &list) != nil {
			res.Message = "The provider answered, but not with a model list; check the URL."
			break
		}
		for _, m := range list.Data {
			if m.ID != "" && len(m.ID) <= 128 && !reflectsModelCredential([]byte(m.ID), credential) {
				res.Models = append(res.Models, m.ID)
			}
		}
		sort.Strings(res.Models)
		res.OK = true
		res.Message = fmt.Sprintf("The key works: the provider lists %d models.", len(res.Models))
	}
	writeJSON(w, 200, res)
}
