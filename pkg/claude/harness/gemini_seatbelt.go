package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// What a launch in Gemini's Seatbelt mode needs beyond the environment
// prefix (gemini_sandbox.go), read from the CLI source at GeminiPinnedVersion
// and the permissive-open profile it ships:
//
//   - The profile denies READING oauth_creds.json, google_accounts.json and
//     gemini-credentials.json anywhere, and the sandboxed child is what talks
//     to the model. A Google sign-in therefore cannot authenticate in there,
//     so the mode needs an auth type whose credential arrives through the
//     environment (an API key, Vertex AI, compute ADC); the parent loads a
//     `.env` into the environment the child inherits.
//   - The profile also denies reading any trustedFolders.json, so the child
//     sees no trust rules at all. With folder trust on (the default) that
//     parks the pane on a trust dialog whose answer cannot be saved, and an
//     untrusted folder downgrades yolo to `default`. Gemini honours
//     GEMINI_CLI_TRUST_WORKSPACE=true ahead of the file, and the child
//     inherits it, so tclaude exports it — but only for a directory the host
//     trust store (which the unsandboxed parent reads) already trusts, or one
//     `--trust-dir` is about to trust. An operator's DO_NOT_TRUST is never
//     overridden.

const geminiTrustWorkspaceEnvVar = "GEMINI_CLI_TRUST_WORKSPACE"

// ValidateGeminiSeatbeltLaunch refuses a Seatbelt-mode launch that would stall:
// one with no selected auth type, or one signed in with Google.
func ValidateGeminiSeatbeltLaunch(getenv func(string) string, cwd string) error {
	if getenv == nil {
		getenv = os.Getenv
	}
	home := geminiLaunchHome(getenv)
	if home == "" {
		return fmt.Errorf("gemini sandbox mode %q: cannot locate the Gemini home (HOME and %s are "+
			"unset or relative)", GeminiSandboxSeatbelt, GeminiHomeEnvVar)
	}
	selected, source, err := geminiSelectedAuthType(getenv, home, cwd)
	if err != nil {
		return fmt.Errorf("gemini sandbox mode %q: %w", GeminiSandboxSeatbelt, err)
	}
	switch selected {
	case "":
		return fmt.Errorf("gemini sandbox mode %q needs a selected auth type "+
			"(settings.json `security.auth.selectedType`); without one the sandboxed pane stops on "+
			"Gemini's sign-in dialog", GeminiSandboxSeatbelt)
	case GeminiAuthLoginWithGoogle:
		return fmt.Errorf("gemini sandbox mode %q cannot use a Google sign-in (%q, from %s): Gemini's "+
			"Seatbelt profile denies reading the cached OAuth credentials, so the sandboxed CLI cannot "+
			"authenticate. Use an API key (%q) or Vertex AI, or use --sandbox-impl tclaude-layer",
			GeminiSandboxSeatbelt, selected, source, GeminiAuthAPIKey)
	}
	return nil
}

// GeminiSeatbeltLaunchEnv returns the environment a Seatbelt-mode launch in cwd
// adds: GEMINI_CLI_TRUST_WORKSPACE=true when the host trust store trusts cwd,
// or when trustDirRequested and no rule distrusts it. Nil otherwise.
func GeminiSeatbeltLaunchEnv(getenv func(string) string, cwd string, trustDirRequested bool) (map[string]string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	trusted, err := geminiHostTrustsDir(getenv, cwd, trustDirRequested)
	if err != nil || !trusted {
		return nil, err
	}
	return map[string]string{geminiTrustWorkspaceEnvVar: "true"}, nil
}

func geminiLaunchHome(getenv func(string) string) string {
	home := strings.TrimSpace(getenv(GeminiHomeEnvVar))
	if home == "" {
		home = strings.TrimSpace(getenv("HOME"))
	}
	if home == "" || !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Clean(home)
}

// geminiHostTrustsDir evaluates the trust store the unsandboxed parent reads,
// with Gemini's rule (trust.ts): the LONGEST matching rule wins, TRUST_FOLDER
// covers its path and below, TRUST_PARENT its parent and below.
func geminiHostTrustsDir(getenv func(string) string, cwd string, trustDirRequested bool) (bool, error) {
	if !filepath.IsAbs(cwd) {
		return false, nil
	}
	path, err := geminiTrustedFoldersPathForLaunch(getenv, geminiLaunchHome(getenv))
	if err != nil {
		return false, err
	}
	rules := map[string]string{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return false, err
	default:
		if json.Unmarshal(stripJSONComments(data), &rules) != nil {
			// Gemini itself refuses to start on this file; claim nothing.
			return false, nil
		}
	}
	dir := filepath.Clean(cwd)
	best, bestLevel := -1, ""
	for rulePath, level := range rules {
		covered := filepath.Clean(rulePath)
		if level == geminiTrustParent {
			covered = filepath.Dir(covered)
		}
		if !geminiPathWithin(dir, covered) || len(covered) <= best {
			continue
		}
		best, bestLevel = len(covered), level
	}
	switch bestLevel {
	case geminiTrustFolder, geminiTrustParent:
		return true, nil
	case geminiDoNotTrust:
		return false, nil
	default:
		return trustDirRequested, nil
	}
}

// geminiPathWithin reports whether dir is root or below it, with the same
// case rule geminiSameTrustPath applies.
func geminiPathWithin(dir, root string) bool {
	if geminiSameTrustPath(dir, root) {
		return true
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	return !geminiSameTrustPath(rel, ".")
}
