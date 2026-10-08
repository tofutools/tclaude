// Package configbundle defines the portable config-bundle container. Individual
// values retain their existing component export formats and version checks.
package configbundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const Format = "tclaude-config-bundle"

var Sections = []string{"roles", "sandbox-profiles", "profiles", "templates", "process-templates", "default-permissions", "config"}

type Item struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
}
type Placeholder struct {
	Name  string `json:"name"`
	Item  string `json:"item"`
	Field string `json:"field"`
}
type Flag struct {
	Item  string `json:"item"`
	Field string `json:"field"`
	Hint  string `json:"hint"`
}
type Bundle struct {
	Format         string            `json:"format"`
	FormatVersion  int               `json:"format_version"`
	CreatedAt      string            `json:"created_at"`
	TclaudeVersion string            `json:"tclaude_version"`
	Sections       map[string][]Item `json:"sections"`
	Placeholders   []Placeholder     `json:"placeholders,omitempty"`
	Flags          []Flag            `json:"flags,omitempty"`
	Omitted        []string          `json:"omitted,omitempty"`
}

func (b *Bundle) Validate() error {
	if b.Format != Format {
		return fmt.Errorf("expected format %q", Format)
	}
	if b.FormatVersion != 1 {
		return fmt.Errorf("unsupported config bundle format_version %d (supported: 1)", b.FormatVersion)
	}
	for section, items := range b.Sections {
		if !slices.Contains(Sections, section) {
			return fmt.Errorf("unknown section %q", section)
		}
		seen := map[string]bool{}
		for _, item := range items {
			if item.Name == "" || strings.Contains(item.Name, "/") || seen[item.Name] {
				return fmt.Errorf("invalid or duplicate item %q in %s", item.Name, section)
			}
			seen[item.Name] = true
			if !json.Valid(item.Value) {
				return fmt.Errorf("invalid value for %s/%s", section, item.Name)
			}
		}
	}
	return nil
}

// Select accepts a section or section/name. Misspellings are errors, to avoid
// accidentally including authority a caller intended to exclude.
func (b *Bundle) Select(only, skip []string) error {
	for _, selector := range append(slices.Clone(only), skip...) {
		section, name, named := strings.Cut(selector, "/")
		if !slices.Contains(Sections, section) {
			return fmt.Errorf("unknown selector %q", selector)
		}
		if named && !slices.ContainsFunc(b.Sections[section], func(i Item) bool { return i.Name == name }) {
			return fmt.Errorf("unknown item %q", selector)
		}
	}
	match := func(selectors []string, section, name string) bool {
		return slices.Contains(selectors, section) || slices.Contains(selectors, section+"/"+name)
	}
	for section, items := range b.Sections {
		kept := []Item{}
		for _, i := range items {
			if (len(only) == 0 || match(only, section, i.Name)) && !match(skip, section, i.Name) {
				kept = append(kept, i)
			}
		}
		b.Sections[section] = kept
	}
	return nil
}

var credential = regexp.MustCompile(`(?i)(?:sk-(?:ant-|proj-)?[a-z0-9_-]{16,}|gh[pousr]_[a-z0-9]{20,}|github_pat_[a-z0-9_]{20,}|(?:token|password|api[_-]?key|secret)\s*[:=]\s*["']?[^\s"']{8,}|-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----|keychain://[^\s]+)`)

func opaqueKey(key string) bool {
	return slices.Contains([]string{"source", "brief", "descr", "initial_message", "startup_context", "default_context", "script", "command", "text", "reason", "disabled_reason"}, key)
}

var secretKey = regexp.MustCompile(`(?i)(token|password|secret|api[_-]?key|private[_-]?key|keychain)`)

// Prepare never edits free text. Structured credentials are omitted; structured
// absolute paths are portable home references or explicit unresolved values.
func (b *Bundle) Prepare() error {
	home, _ := os.UserHomeDir()
	for _, section := range Sections {
		for n, item := range b.Sections[section] {
			label := section + "/" + item.Name
			var value any
			if err := json.Unmarshal(item.Value, &value); err != nil {
				return err
			}
			var walk func(any, string, string) any
			walk = func(v any, field, key string) any {
				switch x := v.(type) {
				case map[string]any:
					if name, ok := x["name"].(string); ok && (secretKey.MatchString(name) || credential.MatchString(fmt.Sprint(x["value"]))) {
						if _, env := x["value"]; env {
							b.Omitted = append(b.Omitted, label+":"+field+" (credential environment entry)")
							return nil
						}
					}
					keys := make([]string, 0, len(x))
					for k := range x {
						keys = append(keys, k)
					}
					slices.Sort(keys)
					for _, k := range keys {
						if secretKey.MatchString(k) && k != "darwin_disable_keychain_write" {
							delete(x, k)
							b.Omitted = append(b.Omitted, label+":"+field+"."+k)
							continue
						}
						x[k] = walk(x[k], field+"."+k, k)
					}
					return x
				case []any:
					out := []any{}
					for j, e := range x {
						y := walk(e, fmt.Sprintf("%s[%d]", field, j), key)
						if y != nil {
							out = append(out, y)
						}
					}
					return out
				case string:
					if credential.MatchString(x) {
						b.Flags = append(b.Flags, Flag{label, field, "suspected credential (value redacted)"})
					}
					// Source, scripts, prose, and context are opaque, never rewritten.
					if opaqueKey(key) {
						return x
					}
					if filepath.IsAbs(x) {
						if home != "" && (x == home || strings.HasPrefix(x, home+string(filepath.Separator))) {
							return "${HOME}" + strings.TrimPrefix(x, home)
						}
						name := fmt.Sprintf("path_%d", len(b.Placeholders)+1)
						b.Placeholders = append(b.Placeholders, Placeholder{name, label, field})
						return "${" + name + "}"
					}
				}
				return v
			}
			value = walk(value, "value", "")
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			b.Sections[section][n].Value = raw
		}
	}
	return nil
}

var variable = regexp.MustCompile(`\$\{([a-zA-Z0-9_]+)\}`)

// Resolve substitutes JSON string values, never JSON syntax. Only selected
// items are inspected, so skipped sections need no machine bindings.
func (b *Bundle) Resolve(values map[string]string) ([]Placeholder, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	missing := []Placeholder{}
	for section, items := range b.Sections {
		for n, item := range items {
			var v any
			if err := json.Unmarshal(item.Value, &v); err != nil {
				return nil, err
			}
			var walk func(any, string, string) any
			walk = func(v any, field, key string) any {
				switch x := v.(type) {
				case map[string]any:
					for k, e := range x {
						x[k] = walk(e, field+"."+k, k)
					}
					return x
				case []any:
					for j, e := range x {
						x[j] = walk(e, fmt.Sprintf("%s[%d]", field, j), key)
					}
					return x
				case string:
					if opaqueKey(key) {
						return x
					}
					return variable.ReplaceAllStringFunc(x, func(token string) string {
						name := variable.FindStringSubmatch(token)[1]
						if name == "HOME" {
							return home
						}
						if val, ok := values[name]; ok {
							return val
						}
						missing = append(missing, Placeholder{name, section + "/" + item.Name, field})
						return token
					})
				}
				return v
			}
			raw, err := json.Marshal(walk(v, "value", ""))
			if err != nil {
				return nil, err
			}
			b.Sections[section][n].Value = raw
		}
	}
	return missing, nil
}
