package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/tofutools/tclaude/internal/backend/host"
	"os"
	"path/filepath"
	"strings"
)

func (p *prepared) ensureLaunchDirectoryTrusted() error {
	root := p.nativeHome
	if root == "" {
		// An unsandboxed launch inherits the daemon environment, with the
		// execution's literal overrides applied in the same order as its command.
		environment := host.MergeEnvironment(os.Environ(), p.command.Env)
		var home string
		for _, entry := range environment {
			key, value, found := strings.Cut(entry, "=")
			if !found {
				continue
			}
			switch key {
			case "CLAUDE_CONFIG_DIR":
				root = value
			case "HOME":
				home = value
			}
		}
		if root == "" {
			root = home
		}
		if root == "" {
			return fmt.Errorf("claude launch has no configuration home")
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(p.request.Spec.WorkingDirectory, root)
		}
	}
	return ensureDirectoryTrusted(root, p.request.Spec.WorkingDirectory)
}

// Keep the v1 exact-number and invalid-string preservation guarantees while
// editing only this directory's native trust record.
func planClaudeDirTrust(data []byte, projectDir string) (bool, []byte, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		// A lone surrogate escape does NOT survive decode→marshal: Go replaces
		// it with U+FFFD, irreversibly. UseNumber protects integers but there is
		// no equivalent knob for strings, so the only safe move on a config
		// carrying one is to leave the file alone — same fail-safe posture as a
		// wrong-shape `projects`. (JSON.stringify emits these only for a split
		// surrogate pair, so a real config is very unlikely to trip this.)
		if err := errOnLoneSurrogateEscape(data); err != nil {
			return false, nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber() // keep big ints exact across the round-trip
		if err := dec.Decode(&root); err != nil {
			return false, nil, fmt.Errorf("parse Claude config: %w", err)
		}
	}

	// `projects` must be a JSON object; absent → create it.
	var projects map[string]any
	switch p := root["projects"].(type) {
	case nil:
		projects = map[string]any{}
		root["projects"] = projects
	case map[string]any:
		projects = p
	default:
		return false, nil, fmt.Errorf("claude dir-trust: `projects` in Claude config is not an object; refusing to edit")
	}

	// The per-dir entry must be an object; absent → create it.
	var entry map[string]any
	switch e := projects[projectDir].(type) {
	case nil:
		entry = map[string]any{}
		projects[projectDir] = entry
	case map[string]any:
		entry = e
	default:
		return false, nil, fmt.Errorf("claude dir-trust: Claude config project entry %q is not an object; refusing to edit", projectDir)
	}

	// Idempotent: already trusted → no rewrite.
	if b, ok := entry["hasTrustDialogAccepted"].(bool); ok && b {
		return false, data, nil
	}
	entry["hasTrustDialogAccepted"] = true

	// json.Marshal escapes <, > and & as </>/& by default, which
	// would rewrite those characters throughout a file tclaude did not author
	// (harmless but gratuitous churn in a diff the operator may well read).
	// Encoder + SetEscapeHTML(false) emits them verbatim; it also appends the
	// trailing newline MarshalIndent does not.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return false, nil, fmt.Errorf("encode Claude config: %w", err)
	}
	return true, buf.Bytes(), nil
}

// errOnLoneSurrogateEscape reports an error when data contains a \uXXXX escape
// that is an UNPAIRED surrogate — a high surrogate (D800-DBFF) not immediately
// followed by a low one, or a low surrogate (DC00-DFFF) not immediately
// preceded by a high one. Such an escape cannot survive Go's decode→encode
// round-trip (it becomes U+FFFD), so the caller refuses the edit rather than
// silently corrupting the operator's config.
//
// Properly PAIRED surrogate escapes round-trip fine and are deliberately
// allowed. Scanning tracks string context so a `\u` inside a comment-free JSON
// document is only read where it can actually be an escape, and consumes
// backslash pairs so a literal `\\u0041` is not mistaken for an escape.
func errOnLoneSurrogateEscape(data []byte) error {
	isHigh := func(r rune) bool { return r >= 0xD800 && r <= 0xDBFF }
	isLow := func(r rune) bool { return r >= 0xDC00 && r <= 0xDFFF }

	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			esc, width, ok := parseUnicodeEscape(data, i)
			if !ok {
				i++ // an ordinary two-byte escape (\" \\ \n …); skip its payload
				continue
			}
			if isHigh(esc) {
				// A valid pair is \uD8xx immediately followed by \uDCxx.
				if next, nextWidth, nextOK := parseUnicodeEscape(data, i+width); nextOK && isLow(next) {
					i += width + nextWidth - 1
					continue
				}
				return fmt.Errorf("claude dir-trust: Claude config contains an unpaired high-surrogate escape (\\u%04X) that would be corrupted by a rewrite; refusing to edit", esc)
			}
			if isLow(esc) {
				return fmt.Errorf("claude dir-trust: Claude config contains an unpaired low-surrogate escape (\\u%04X) that would be corrupted by a rewrite; refusing to edit", esc)
			}
			i += width - 1
		}
	}
	return nil
}

// parseUnicodeEscape decodes a `\uXXXX` escape starting at data[i] (which must
// be the backslash), returning the code unit and the escape's byte width.
// ok=false when data[i:] is not a well-formed \u escape — including an
// ordinary escape like \n, which the caller handles separately.
func parseUnicodeEscape(data []byte, i int) (esc rune, width int, ok bool) {
	if i+5 >= len(data) || data[i] != '\\' || data[i+1] != 'u' {
		return 0, 0, false
	}
	var v rune
	for _, b := range data[i+2 : i+6] {
		switch {
		case b >= '0' && b <= '9':
			v = v<<4 | rune(b-'0')
		case b >= 'a' && b <= 'f':
			v = v<<4 | rune(b-'a'+10)
		case b >= 'A' && b <= 'F':
			v = v<<4 | rune(b-'A'+10)
		default:
			return 0, 0, false
		}
	}
	return v, 6, true
}

func ensureDirectoryTrusted(root, cwd string) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(cwd) {
		return fmt.Errorf("directory trust requires absolute native root and working directory")
	}
	return host.EditNativeConfigFile("claude config", filepath.Join(root, ".claude.json"), 0600, func(data []byte) (bool, []byte, error) { return planClaudeDirTrust(data, cwd) })
}
