package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Gemini CLI directory trust.
//
// Read from packages/core/src/utils/trust.ts at GeminiPinnedVersion, not
// measured on a live pane (no account was available). Folder trust is ON by
// default, and a launch in a folder with no matching rule stops on a trust
// dialog. While a folder is untrusted, Gemini also disables settings hooks
// (so tclaude gets no live status) and yolo/auto_edit approval, and headless
// mode — the `ask` path — exits with an error instead of asking.
//
// The store is a flat JSON object mapping a path to a trust level:
//
//	{ "/abs/project": "TRUST_FOLDER", "/abs/other": "DO_NOT_TRUST" }
//
// at GEMINI_CLI_TRUSTED_FOLDERS_PATH when set, else
// <GEMINI_CLI_HOME or HOME>/.gemini/trustedFolders.json. A rule covers its
// path and everything below it (TRUST_PARENT covers the rule's parent), and
// the LONGEST matching rule wins. A file Gemini cannot parse, or a value that
// is not a trust level, is a FATAL startup error for every Gemini launch — so
// this editor refuses any shape it does not fully understand rather than risk
// writing one.
//
// Like every other harness's editor, it is reached only through
// EnsureDirTrusted on an explicit opt-in, and it pre-answers the trust
// question for exactly one directory. It never overrides an operator's
// explicit DO_NOT_TRUST for that same directory.

const (
	// GeminiTrustedFoldersPathEnvVar relocates the trust store
	// (Storage.getTrustedFoldersPath).
	GeminiTrustedFoldersPathEnvVar = "GEMINI_CLI_TRUSTED_FOLDERS_PATH"
	geminiTrustedFoldersFileName   = "trustedFolders.json"

	geminiTrustFolder  = "TRUST_FOLDER"
	geminiTrustParent  = "TRUST_PARENT"
	geminiDoNotTrust   = "DO_NOT_TRUST"
	geminiTrustStoreUI = "~/.gemini/trustedFolders.json"
)

// geminiTrustedFoldersPathForLaunch resolves the trust store a launch with the
// given environment reads. A relative override is refused: Gemini would
// resolve it against the pane's cwd, tclaude against its own.
func geminiTrustedFoldersPathForLaunch(getenv func(string) string, home string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if override := strings.TrimSpace(getenv(GeminiTrustedFoldersPathEnvVar)); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("gemini dir-trust: %s=%q is not absolute", GeminiTrustedFoldersPathEnvVar, override)
		}
		return filepath.Clean(override), nil
	}
	base := strings.TrimSpace(getenv(GeminiHomeEnvVar))
	if base == "" {
		base = strings.TrimSpace(home)
	}
	if base == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("gemini dir-trust: cannot determine home dir: %w", err)
		}
		base = resolved
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("gemini dir-trust: home %q is not absolute", base)
	}
	return filepath.Join(filepath.Clean(base), geminiDirName, geminiTrustedFoldersFileName), nil
}

// EnsureGeminiDirTrustedForLaunch records projectDir as TRUST_FOLDER in the
// trust store the launch will read. getenv nil and home "" mean the ambient
// environment.
func EnsureGeminiDirTrustedForLaunch(getenv func(string) string, home, projectDir string) error {
	if !filepath.IsAbs(projectDir) {
		return fmt.Errorf("gemini dir-trust: project dir %q is not absolute", projectDir)
	}
	path, err := geminiTrustedFoldersPathForLaunch(getenv, home)
	if err != nil {
		return err
	}
	dir := filepath.Clean(projectDir)
	return editHarnessConfigFile("Gemini trusted folders", path, 0o600,
		func(data []byte) (bool, []byte, error) {
			return planGeminiDirTrust(data, dir)
		}, prepareAtomicWriteFile)
}

// planGeminiDirTrust adds dir as TRUST_FOLDER. It returns changed=false when
// the exact path already carries a trusting rule.
func planGeminiDirTrust(data []byte, dir string) (bool, []byte, error) {
	rules := map[string]string{}
	if len(bytes.TrimSpace(data)) > 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
			return false, nil, fmt.Errorf("gemini dir-trust: %s is not a strict JSON object "+
				"(tclaude does not rewrite a file with comments); trust the folder once in Gemini instead",
				geminiTrustedFoldersFileName)
		}
		for path, value := range raw {
			var level string
			if json.Unmarshal(value, &level) != nil || !geminiIsTrustLevel(level) {
				return false, nil, fmt.Errorf("gemini dir-trust: %s has an entry for %q that is not a "+
					"trust level; Gemini itself refuses to start with it, so fix the file first",
					geminiTrustedFoldersFileName, path)
			}
			rules[path] = level
		}
	}
	for path, level := range rules {
		if !geminiSameTrustPath(path, dir) {
			continue
		}
		switch level {
		case geminiDoNotTrust:
			return false, nil, fmt.Errorf("gemini dir-trust: %s marks %s as DO_NOT_TRUST; "+
				"tclaude will not override an explicit distrust", geminiTrustedFoldersFileName, dir)
		case geminiTrustFolder, geminiTrustParent:
			return false, nil, nil
		}
	}
	rules[dir] = geminiTrustFolder
	out, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return false, nil, err
	}
	return true, append(out, '\n'), nil
}

func geminiIsTrustLevel(level string) bool {
	switch level {
	case geminiTrustFolder, geminiTrustParent, geminiDoNotTrust:
		return true
	}
	return false
}

// geminiSameTrustPath compares two rule paths the way Gemini's normalizePath
// does: case-insensitively on macOS, where the CLI lowercases every key it
// loads and saves, and exactly elsewhere. Without it a Gemini-written
// lowercase DO_NOT_TRUST would go unseen, and tclaude would report a folder
// trusted that Gemini still distrusts.
func geminiSameTrustPath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
