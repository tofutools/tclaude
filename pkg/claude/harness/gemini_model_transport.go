package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Gemini CLI's filtered-network model route.
//
// Read from the upstream source at GeminiPinnedVersion, not measured against a
// live account: an interactive Gemini launch authenticates with
// settings.json `security.auth.selectedType` — with no selected type it stops
// on its auth dialog — and each type has a fixed first-party route:
//
//   - gemini-api-key  → generativelanguage.googleapis.com
//     (loggingContentGenerator's "public Gemini API endpoint")
//   - oauth-personal  → cloudcode-pa.googleapis.com, the Code Assist server
//     (code_assist/server.ts CODE_ASSIST_ENDPOINT), plus oauth2.googleapis.com,
//     where google-auth-library refreshes the cached login's access token
//
// Every other type (vertex-ai, compute-default-credentials, gateway, the
// legacy cloud-shell) and every variable that moves a route
// (CODE_ASSIST_ENDPOINT, GOOGLE_GEMINI_BASE_URL, GOOGLE_VERTEX_BASE_URL) is
// REFUSED rather than followed: Vertex's host depends on region and routing
// configuration this resolver does not model, and a user-chosen base URL is an
// unreviewed endpoint. Gemini also loads one `.env` file into its own
// environment at startup (settings.ts findEnvFile), without overriding what
// the launch already set, so a route variable or a proxy in that file moves the
// route just as surely and is refused the same way.
//
// The honest limit, as for every harness: this names what a launch NEEDS for
// model traffic. Telemetry (play.googleapis.com), web fetch and search, MCP
// servers and the first sign-in (a browser OAuth flow) are separate features
// with their own destinations.

const (
	// GeminiAuthAPIKey and GeminiAuthLoginWithGoogle are the two
	// security.auth.selectedType values with a reviewed filtered route.
	GeminiAuthAPIKey          = "gemini-api-key"
	GeminiAuthLoginWithGoogle = "oauth-personal"

	// GeminiAPINetworkPack and GeminiLoginNetworkPack are the release-owned
	// packs covering the two routes, named as the requirement's Template so a
	// refusal can say "include template …".
	GeminiAPINetworkPack   = "net-google-gemini-api"
	GeminiLoginNetworkPack = "net-google-gemini-login"

	geminiAPIHost        = "generativelanguage.googleapis.com"
	geminiCodeAssistHost = "cloudcode-pa.googleapis.com"
	googleOAuthTokenHost = "oauth2.googleapis.com"

	geminiSystemSettingsEnvVar = "GEMINI_CLI_SYSTEM_SETTINGS_PATH"
	geminiSystemDefaultsEnvVar = "GEMINI_CLI_SYSTEM_DEFAULTS_PATH"
)

// GeminiAPIKeyDestinations and GeminiLoginDestinations are the destination
// sets of the two routes, shared by the resolver and the network packs so the
// two cannot drift.
func GeminiAPIKeyDestinations() []sandboxpolicy.NetworkAllowEntry {
	return []sandboxpolicy.NetworkAllowEntry{{Domain: geminiAPIHost, Ports: []int{443}}}
}

func GeminiLoginDestinations() []sandboxpolicy.NetworkAllowEntry {
	return []sandboxpolicy.NetworkAllowEntry{
		{Domain: geminiCodeAssistHost, Ports: []int{443}},
		{Domain: googleOAuthTokenHost, Ports: []int{443}},
	}
}

// geminiRouteMovingEnvVars change which endpoint Gemini talks to. Their values
// are never followed.
var geminiRouteMovingEnvVars = []string{
	"CODE_ASSIST_ENDPOINT",
	"GOOGLE_GEMINI_BASE_URL",
	"GOOGLE_VERTEX_BASE_URL",
	// Rewrites every *.googleapis.com host the Google client libraries use.
	"GOOGLE_CLOUD_UNIVERSE_DOMAIN",
}

// GeminiRouteMovingEnvVars returns a fresh copy of the refused variables.
func GeminiRouteMovingEnvVars() []string {
	return append([]string(nil), geminiRouteMovingEnvVars...)
}

// geminiModelTransport is Gemini's ModelTransportResolver. Like Copilot's, it
// does not follow a resolved BaseURL: a non-default endpoint is what the
// launch resolver refuses.
type geminiModelTransport struct{}

func (geminiModelTransport) ResolveModelTransport(
	resolved ResolvedModelTransport,
) (ModelTransportRequirement, error) {
	if !resolved.ProviderResolved {
		return ModelTransportRequirement{}, fmt.Errorf("gemini auth configuration was not resolved")
	}
	if endpoint := strings.TrimSpace(resolved.BaseURL); endpoint != "" {
		return ModelTransportRequirement{}, fmt.Errorf(
			"this Gemini launch resolves to custom endpoint %q, which has no reviewed filtered-network "+
				"route; use the default Gemini API or Google sign-in route, or use network open", endpoint)
	}
	switch strings.TrimSpace(resolved.Provider) {
	case GeminiAuthAPIKey:
		return ModelTransportRequirement{
			Template:     GeminiAPINetworkPack,
			Destinations: GeminiAPIKeyDestinations(),
			ResolvedBy:   "Gemini API-key route (harness default endpoint)",
		}, nil
	case GeminiAuthLoginWithGoogle:
		return ModelTransportRequirement{
			Template:     GeminiLoginNetworkPack,
			Destinations: GeminiLoginDestinations(),
			ResolvedBy:   "Gemini Google sign-in route (Code Assist plus token refresh)",
		}, nil
	default:
		return ModelTransportRequirement{}, fmt.Errorf(
			"Gemini auth type %q has no reviewed filtered-network route; use %q or %q, or use network open",
			resolved.Provider, GeminiAuthAPIKey, GeminiAuthLoginWithGoogle)
	}
}

// ResolveGeminiLaunchAuthType returns the auth type a Gemini launch in cwd
// will use, after refusing the inputs that would move its route. getenv is
// the launch environment.
func ResolveGeminiLaunchAuthType(getenv func(string) string, cwd string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, variable := range geminiRouteMovingEnvVars {
		if strings.TrimSpace(getenv(variable)) != "" {
			return "", fmt.Errorf("Gemini launch variable %s moves the model route away from the "+
				"default endpoint, and tclaude has no reviewed filtered-network resolver for it; "+
				"remove it, or use network open", variable)
		}
	}
	home := strings.TrimSpace(getenv(GeminiHomeEnvVar))
	if home == "" {
		home = strings.TrimSpace(getenv("HOME"))
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("cannot locate the Gemini home because HOME and %s are unset or "+
			"relative, so the launch's auth configuration cannot be resolved; set %s or use network open",
			GeminiHomeEnvVar, GeminiHomeEnvVar)
	}
	if err := geminiCheckDotEnv(getenv, home, cwd); err != nil {
		return "", err
	}

	selected, source, err := geminiSelectedAuthType(getenv, home, cwd)
	if err != nil {
		return "", err
	}
	switch selected {
	case "":
		return "", errors.New("no Gemini auth type is selected (settings.json " +
			"`security.auth.selectedType`), so the pane would stop on Gemini's sign-in dialog and its " +
			"model route is unknown; sign in once in an interactive Gemini session, or use network open")
	case GeminiAuthAPIKey, GeminiAuthLoginWithGoogle:
		return selected, nil
	default:
		return "", fmt.Errorf("Gemini auth type %q (from %s) has no reviewed filtered-network route; "+
			"use %q or %q, or use network open", selected, source, GeminiAuthAPIKey, GeminiAuthLoginWithGoogle)
	}
}

// geminiSelectedAuthType reads security.auth.selectedType from Gemini's merged
// settings, returning the value and the file that set it ("" when none did).
func geminiSelectedAuthType(getenv func(string) string, home, cwd string) (selected, source string, err error) {
	// Gemini's merge order, lowest first: system defaults, user, workspace,
	// system (settings.ts loadSettings). Workspace settings apply only in a
	// trusted folder; they are honoured here regardless, since a daemon
	// launch runs in a trusted folder, and a misread route is denied at the
	// wall rather than widened.
	systemPath := strings.TrimSpace(getenv(geminiSystemSettingsEnvVar))
	if systemPath == "" {
		systemPath = geminiDefaultSystemSettingsPath()
	}
	defaultsPath := strings.TrimSpace(getenv(geminiSystemDefaultsEnvVar))
	if defaultsPath == "" {
		defaultsPath = filepath.Join(filepath.Dir(systemPath), "system-defaults.json")
	}
	paths := []string{defaultsPath, filepath.Join(home, geminiDirName, "settings.json")}
	if cwd != "" && filepath.IsAbs(cwd) {
		paths = append(paths, filepath.Join(cwd, geminiDirName, "settings.json"))
	}
	paths = append(paths, systemPath)

	for _, path := range paths {
		value, set, err := geminiSettingsSelectedAuthType(path)
		if err != nil {
			return "", "", err
		}
		if set {
			selected, source = value, path
		}
	}
	return selected, source, nil
}

func geminiDefaultSystemSettingsPath() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/GeminiCli/settings.json"
	}
	return "/etc/gemini-cli/settings.json"
}

// geminiSettingsSelectedAuthType reads security.auth.selectedType from one
// settings file. A missing file is (_, false, nil); a file Gemini would parse
// but tclaude cannot is an error, never a guess.
func geminiSettingsSelectedAuthType(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("cannot read Gemini settings %s to resolve the model route: %v; "+
			"fix its permissions or use network open", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "", false, nil
	}
	var settings struct {
		Security *struct {
			Auth *struct {
				SelectedType *string `json:"selectedType"`
			} `json:"auth"`
		} `json:"security"`
	}
	if err := json.Unmarshal(stripJSONComments(data), &settings); err != nil {
		return "", false, fmt.Errorf("cannot parse Gemini settings %s to resolve the model route: %v; "+
			"fix the file or use network open", path, err)
	}
	if settings.Security == nil || settings.Security.Auth == nil || settings.Security.Auth.SelectedType == nil {
		return "", false, nil
	}
	return strings.TrimSpace(*settings.Security.Auth.SelectedType), true, nil
}

// geminiCheckDotEnv refuses a launch whose `.env` would move the route. It
// mirrors findEnvFile without knowing the folder's trust: both the file a
// trusted folder would load and the one an untrusted folder would load are
// checked, which can only over-refuse.
func geminiCheckDotEnv(getenv func(string) string, home, cwd string) error {
	refused := append(GeminiRouteMovingEnvVars(), ModelTransportProxyEnvVars()...)
	for _, trusted := range []bool{true, false} {
		path := geminiFindEnvFile(home, cwd, trusted)
		if path == "" {
			continue
		}
		values, err := readGeminiDotEnv(path)
		if err != nil {
			return fmt.Errorf("cannot read %s, which Gemini loads into its environment, to resolve "+
				"the model route: %v; fix the file or use network open", path, err)
		}
		for _, variable := range refused {
			// The launch environment wins: Gemini never overrides a set variable.
			if strings.TrimSpace(values[variable]) == "" || getenv(variable) != "" {
				continue
			}
			return fmt.Errorf("%s sets %s, which Gemini loads into its environment and which moves "+
				"the model route away from the default endpoint; remove it, or use network open",
				path, variable)
		}
	}
	return nil
}

// geminiFindEnvFile is settings.ts findEnvFile.
func geminiFindEnvFile(home, cwd string, trusted bool) string {
	exists := func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && !info.IsDir()
	}
	dir := filepath.Clean(cwd)
	if cwd == "" || !filepath.IsAbs(dir) {
		dir = home
	}
	for {
		if trusted {
			if path := filepath.Join(dir, geminiDirName, ".env"); exists(path) {
				return path
			}
		}
		if path := filepath.Join(dir, ".env"); exists(path) {
			return path
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if trusted {
		if path := filepath.Join(home, geminiDirName, ".env"); exists(path) {
			return path
		}
	}
	if path := filepath.Join(home, ".env"); exists(path) {
		return path
	}
	return ""
}

// geminiDotEnvLine is the bundled dotenv's LINE grammar, reduced to what this
// check needs: an optional `export` with any whitespace, a [\w.-]+ key, and
// either `=` or the `KEY: value` colon form.
var geminiDotEnvLine = regexp.MustCompile(`^\s*(?:export\s+)?([\w.-]+)(?:\s*=\s*|:\s+)(.*)$`)

// readGeminiDotEnv reads the variables a dotenv file sets. Values are only
// tested for being non-empty, so a quoted value is unwrapped and an unquoted
// one is cut at an inline ` #` comment; a multi-line quoted value counts by
// its first line.
func readGeminiDotEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		match := geminiDotEnvLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		key, value := match[1], strings.TrimSpace(match[2])
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'' || value[0] == '`') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		} else if at := strings.Index(value, " #"); at >= 0 {
			value = strings.TrimSpace(value[:at])
		}
		values[key] = value
	}
	return values, scanner.Err()
}

// ModelTransportProxyEnvVars names the proxy variables that hide a launch's
// real destination: the launch resolver's refusal list
// (session.ModelTransportProxyVariables), and what a Gemini `.env` may not set.
func ModelTransportProxyEnvVars() []string {
	return []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"}
}

// stripJSONComments removes `//` and `/* */` comments outside strings, the
// strip-json-comments transform Gemini applies before JSON.parse. Comment
// bytes become spaces so error offsets still point at the original text.
func stripJSONComments(data []byte) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	inString, escaped := false, false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
		case c == '/' && i+1 < len(out) && out[i+1] == '/':
			for ; i < len(out) && out[i] != '\n'; i++ {
				out[i] = ' '
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			out[i], out[i+1] = ' ', ' '
			for i += 2; i < len(out); i++ {
				if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
					out[i], out[i+1] = ' ', ' '
					i++
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
			}
		}
	}
	return out
}
