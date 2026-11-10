package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// A new config key must be given a class on purpose: unclassified, it would
// be neither remotely writable nor deliberately local.
func TestEveryConfigKeyHasAClass(t *testing.T) {
	leaves := configLeafPaths()
	if len(leaves) < 100 {
		t.Fatalf("the config walk found only %d keys", len(leaves))
	}
	for _, leaf := range leaves {
		if _, ok := configKeyClass(leaf); !ok {
			t.Errorf("config key %s has no class in configKeyClasses (config_classes.go): decide which peer grant may change it", leaf)
		}
	}
}

// A stale entry would silently classify a future key of the same name.
func TestConfigClassEntriesNameRealKeys(t *testing.T) {
	leaves := configLeafPaths()
	seen := map[string]bool{}
	for _, e := range configKeyClasses {
		if seen[e.prefix] {
			t.Errorf("configKeyClasses lists %s twice", e.prefix)
		}
		seen[e.prefix] = true
		if !slices.ContainsFunc(leaves, func(l string) bool { return l == e.prefix || strings.HasPrefix(l, e.prefix+".") }) {
			t.Errorf("configKeyClasses entry %s matches no config key", e.prefix)
		}
	}
}

func TestConfigSecretPathsLieInKeys(t *testing.T) {
	for _, p := range configSecretPaths {
		if _, ok := slices.BinarySearch(configLeafPaths(), configSecretPathRoot(p)); !ok {
			t.Errorf("secret path %s lies in no config key (root %s)", p, configSecretPathRoot(p))
		}
	}
	// Each secret is classified: shown as secret in the table the UI gets.
	for _, k := range configKeyTable() {
		if k.Path == "federation.invite" && !k.Secret {
			t.Error("federation.invite is not marked secret in the key table")
		}
	}
}

// The config bundle's portable keys travel between nodes as plain values, so
// none of them may hold a secret.
func TestConfigBundleKeysCarryNoSecret(t *testing.T) {
	for _, key := range bundleConfigKeys {
		for _, p := range configSecretPaths {
			root := configSecretPathRoot(p)
			if root == key || strings.HasPrefix(root, key+".") {
				t.Errorf("bundle config key %s carries the secret %s", key, p)
			}
		}
	}
}

func TestConfigKeyClassLongestPrefixWins(t *testing.T) {
	for path, want := range map[string]configClass{
		"agent.default_permissions":          configClassAgents,
		"agent.context_nudge.enabled":        configClassGeneral,
		"agent.git_proxy.ssh_key":            configClassCredentials,
		"agent.dashboard_bind":               configClassNetwork,
		"federation.hub_url":                 configClassLocal,
		"federation.name":                    configClassNetwork,
		"notifications.notification_command": configClassLocal,
		"notifications.enabled":              configClassGeneral,
		"updates.self.method":                configClassInstallation,
		"updates.self.custom.command":        configClassLocal,
		"audit.retention_days":               configClassLocal,
	} {
		if got, _ := configKeyClass(path); got != want {
			t.Errorf("%s: class %s, want %s", path, got, want)
		}
	}
}

// The local tab answers the shape a peer gets, with every class writable,
// and takes a partial update too.
func TestLocalConfigAccessAndPatch(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	w := httptest.NewRecorder()
	serveDashboardConfig(w, dashboardRequest(http.MethodGet, "/api/config", ""))
	require.Equal(t, 200, w.Code, w.Body.String())
	var v struct {
		Version string `json:"version"`
		Access  struct {
			Level   string            `json:"level"`
			Classes map[string]string `json:"classes"`
		} `json:"access"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
	require.Equal(t, "local", v.Access.Level)
	require.Equal(t, "write", v.Access.Classes["local"])
	w = httptest.NewRecorder()
	serveDashboardConfig(w, dashboardRequest(http.MethodPatch, "/api/config", `{"version":"`+v.Version+`","set":{"log_level":"debug","federation.invite":"tok"}}`))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "tok", "the node's own operator sees its values")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, "debug", cfg.LogLevel)
	require.Equal(t, "tok", cfg.Federation.Invite)
}

// A struct with its own JSON form is one key, not skipped.
func TestConfigLeavesKeepTimeValues(t *testing.T) {
	for _, want := range []string{"federation.away.since", "federation.away.until"} {
		if _, ok := slices.BinarySearch(configLeafPaths(), want); !ok {
			t.Errorf("%s is not a config key", want)
		}
	}
}

// A repeated or empty list identity cannot carry a withheld secret: a peer
// change is refused rather than writing one element's value into another.
func TestPeerConfigChangeRefusesAmbiguousIdentity(t *testing.T) {
	current := config.DefaultConfig()
	current.Service = &config.ServiceConfig{Launch: &config.LaunchProfile{Env: []config.LaunchEnv{{Key: "K", Value: "a"}, {Key: "K", Value: "b"}}}}
	c := configCaller{actor: "operator@peer", peer: "peer", unrestricted: true}
	raw, _, err := configViewRaw(current, c)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &m))
	m["log_level"] = "debug"
	body, err := json.Marshal(m)
	require.NoError(t, err)
	_, _, _, err = computeConfigChange(current, configChangeBody{Config: body, Base: raw}, false, c)
	var refusal *configRefusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, "ambiguous_identity", refusal.body["code"])
	require.Equal(t, "a", current.Service.Launch.Env[0].Value)
}
