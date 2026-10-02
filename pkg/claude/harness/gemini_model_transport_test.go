package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The packs live in the policy package, which cannot import this one, so only
// this test keeps a selectable pack from drifting off the resolver's route.
func TestGeminiNetworkPacksMatchTheResolver(t *testing.T) {
	for pack, want := range map[string][]sandboxpolicy.NetworkAllowEntry{
		GeminiAPINetworkPack:   GeminiAPIKeyDestinations(),
		GeminiLoginNetworkPack: GeminiLoginDestinations(),
	} {
		entries, err := sandboxpolicy.ExpandNetworkPackEntries(pack)
		require.NoError(t, err)
		require.Lenf(t, entries, len(want), "pack %s", pack)
		for _, required := range want {
			assert.Truef(t, slices.ContainsFunc(entries, func(entry sandboxpolicy.NetworkAllowEntry) bool {
				return strings.EqualFold(entry.Domain, required.Domain) && slices.Equal(entry.Ports, required.Ports)
			}), "pack %s does not cover %s", pack, required.Domain)
		}
	}
}

func TestGeminiModelTransportRoutes(t *testing.T) {
	h := MustGet(GeminiName)
	for authType, pack := range map[string]string{
		GeminiAuthAPIKey:          GeminiAPINetworkPack,
		GeminiAuthLoginWithGoogle: GeminiLoginNetworkPack,
	} {
		requirement, err := ResolveModelTransportRequirement(h,
			ResolvedModelTransport{Provider: authType, ProviderResolved: true})
		require.NoError(t, err)
		assert.Equal(t, pack, requirement.Template)
	}
	_, err := ResolveModelTransportRequirement(h,
		ResolvedModelTransport{Provider: "vertex-ai", ProviderResolved: true})
	assert.ErrorContains(t, err, "no reviewed filtered-network route")
	_, err = ResolveModelTransportRequirement(h,
		ResolvedModelTransport{Provider: GeminiAuthAPIKey, BaseURL: "https://gw.example", ProviderResolved: true})
	assert.ErrorContains(t, err, "custom endpoint")
}

type geminiRouteFixture struct {
	home, cwd string
	env       map[string]string
}

func newGeminiRouteFixture(t *testing.T) *geminiRouteFixture {
	t.Helper()
	root := t.TempDir()
	fx := &geminiRouteFixture{
		home: filepath.Join(root, "home"),
		cwd:  filepath.Join(root, "work", "proj"),
		env:  map[string]string{},
	}
	require.NoError(t, os.MkdirAll(filepath.Join(fx.home, ".gemini"), 0o755))
	require.NoError(t, os.MkdirAll(fx.cwd, 0o755))
	fx.env["HOME"] = fx.home
	// Hermetic: never read the host's /etc/gemini-cli.
	fx.env[geminiSystemSettingsEnvVar] = filepath.Join(root, "system", "settings.json")
	return fx
}

func (fx *geminiRouteFixture) write(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

func (fx *geminiRouteFixture) resolve() (string, error) {
	return ResolveGeminiLaunchAuthType(func(name string) string { return fx.env[name] }, fx.cwd)
}

func TestResolveGeminiLaunchAuthType(t *testing.T) {
	t.Run("user settings with comments select the route", func(t *testing.T) {
		fx := newGeminiRouteFixture(t)
		fx.write(t, filepath.Join(fx.home, ".gemini", "settings.json"),
			"{\n  // signed in once\n  \"security\": {\"auth\": {/* type */ \"selectedType\": \"oauth-personal\"}},\n"+
				"  \"url\": \"https://example.com//not-a-comment\"\n}")
		got, err := fx.resolve()
		require.NoError(t, err)
		assert.Equal(t, GeminiAuthLoginWithGoogle, got)
	})
	t.Run("workspace overrides user, system overrides both", func(t *testing.T) {
		fx := newGeminiRouteFixture(t)
		fx.write(t, filepath.Join(fx.home, ".gemini", "settings.json"),
			`{"security":{"auth":{"selectedType":"oauth-personal"}}}`)
		fx.write(t, filepath.Join(fx.cwd, ".gemini", "settings.json"),
			`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`)
		got, err := fx.resolve()
		require.NoError(t, err)
		assert.Equal(t, GeminiAuthAPIKey, got)
		fx.write(t, fx.env[geminiSystemSettingsEnvVar], `{"security":{"auth":{"selectedType":"vertex-ai"}}}`)
		_, err = fx.resolve()
		assert.ErrorContains(t, err, `"vertex-ai"`)
	})
	t.Run("no selected type is refused", func(t *testing.T) {
		fx := newGeminiRouteFixture(t)
		_, err := fx.resolve()
		assert.ErrorContains(t, err, "sign-in dialog")
	})
	t.Run("a route variable in the launch is refused", func(t *testing.T) {
		for _, variable := range GeminiRouteMovingEnvVars() {
			fx := newGeminiRouteFixture(t)
			fx.write(t, filepath.Join(fx.home, ".gemini", "settings.json"),
				`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`)
			fx.env[variable] = "https://gw.example"
			_, err := fx.resolve()
			assert.ErrorContainsf(t, err, variable, "variable %s", variable)
		}
	})
	t.Run("a route variable or proxy in the loaded .env is refused", func(t *testing.T) {
		fx := newGeminiRouteFixture(t)
		fx.write(t, filepath.Join(fx.home, ".gemini", "settings.json"),
			`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`)
		env := filepath.Join(filepath.Dir(fx.cwd), ".env")
		fx.write(t, env, "# project env\nexport GOOGLE_GEMINI_BASE_URL='https://gw.example'\n")
		_, err := fx.resolve()
		assert.ErrorContains(t, err, "GOOGLE_GEMINI_BASE_URL")

		// dotenv's colon form and a tab after export are variables too.
		fx.write(t, env, "export\tGOOGLE_CLOUD_UNIVERSE_DOMAIN=example.test\n")
		_, err = fx.resolve()
		assert.ErrorContains(t, err, "GOOGLE_CLOUD_UNIVERSE_DOMAIN")
		fx.write(t, env, "CODE_ASSIST_ENDPOINT: https://gw.example\n")
		_, err = fx.resolve()
		assert.ErrorContains(t, err, "CODE_ASSIST_ENDPOINT")

		// The trusted-folder .gemini/.env shadows it; a proxy there is refused.
		fx.write(t, filepath.Join(fx.cwd, ".gemini", ".env"), "HTTPS_PROXY=http://proxy.example:8080\n")
		_, err = fx.resolve()
		assert.ErrorContains(t, err, "HTTPS_PROXY")

		// A value the launch itself sets wins over the file.
		fx.env["HTTPS_PROXY"] = ""
		require.NoError(t, os.Remove(env))
		fx.write(t, filepath.Join(fx.cwd, ".gemini", ".env"), "GEMINI_API_KEY=abc\n")
		got, err := fx.resolve()
		require.NoError(t, err)
		assert.Equal(t, GeminiAuthAPIKey, got)
	})
	t.Run("unparsable settings are refused, not guessed", func(t *testing.T) {
		fx := newGeminiRouteFixture(t)
		fx.write(t, filepath.Join(fx.home, ".gemini", "settings.json"), `{"security": {`)
		_, err := fx.resolve()
		assert.ErrorContains(t, err, "cannot parse Gemini settings")
	})
}

// Gemini is enforced by the Linux packet gateway like Claude Code and Codex,
// and nowhere else.
func TestGeminiFilteredNetworkUsesThePacketGateway(t *testing.T) {
	h := MustGet(GeminiName)
	axes := sandboxpolicy.ResolvedAxes{Network: sandboxpolicy.NetworkRules{
		Mode:  sandboxpolicy.AccessModeList,
		Allow: GeminiAPIKeyDestinations(),
	}}
	row, err := accessEnforcementTable(h, sandboxpolicy.ImplementationTclaudeLayer, axes,
		GeminiSandboxOff, "linux", true)
	require.NoError(t, err)
	assert.Equal(t, EnforceFull, row.NetworkList)
	assert.Contains(t, row.Mechanism, "gateway")

	row, err = accessEnforcementTable(h, sandboxpolicy.ImplementationTclaudeLayer, axes,
		GeminiSandboxOff, "linux", false)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, row.NetworkList, "without the gateway prerequisites nothing is claimed")

	row, err = accessEnforcementTable(h, sandboxpolicy.ImplementationTclaudeLayer, axes,
		GeminiSandboxOff, "darwin", true)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, row.NetworkList, "the gateway is Linux-only")
}
