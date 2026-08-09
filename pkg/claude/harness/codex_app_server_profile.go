package harness

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// CodexAppServerProfileOverrides converts a freshly generated managed profile
// into an equivalent app-server config overlay. Codex 0.147 deliberately
// rejects the runtime-only `-p` selector for `app-server`, while `-c` is part
// of the app-server command contract. The permission table is supplied as one
// inline value so an identically named table in user config cannot contribute
// extra fields and widen the launch posture.
func CodexAppServerProfileOverrides(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read managed Codex app-server profile: %w", err)
	}
	var config struct {
		DefaultPermissions string `toml:"default_permissions"`
		Features           struct {
			NetworkProxy      *bool `toml:"network_proxy"`
			UseLegacyLandlock *bool `toml:"use_legacy_landlock"`
		} `toml:"features"`
		Permissions map[string]struct {
			Extends    string            `toml:"extends"`
			Filesystem map[string]string `toml:"filesystem"`
			Network    struct {
				Enabled     bool              `toml:"enabled"`
				UnixSockets map[string]string `toml:"unix_sockets"`
			} `toml:"network"`
		} `toml:"permissions"`
	}
	decoder := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode managed Codex app-server profile: %w", err)
	}
	name, err := ValidateCodexProfileName(config.DefaultPermissions)
	if err != nil || name == "" {
		return nil, fmt.Errorf("managed Codex app-server profile has invalid default_permissions %q", config.DefaultPermissions)
	}
	profile, ok := config.Permissions[name]
	if !ok {
		return nil, fmt.Errorf("managed Codex app-server profile is missing permissions.%s", name)
	}
	if profile.Extends != ":workspace" {
		return nil, fmt.Errorf("managed Codex app-server profile %s has unexpected base %q", name, profile.Extends)
	}

	var table strings.Builder
	table.WriteString(`{extends=":workspace",filesystem={`)
	paths := sortedMapKeys(profile.Filesystem)
	for i, path := range paths {
		access := profile.Filesystem[path]
		if access != "read" && access != "write" && access != "none" {
			return nil, fmt.Errorf("managed Codex app-server profile has invalid filesystem access %q", access)
		}
		if i > 0 {
			table.WriteByte(',')
		}
		table.WriteString(codexTOMLString(path))
		table.WriteByte('=')
		table.WriteString(codexTOMLString(access))
	}
	table.WriteString(`},network={enabled=`)
	table.WriteString(strconv.FormatBool(profile.Network.Enabled))
	table.WriteString(`,unix_sockets={`)
	paths = sortedMapKeys(profile.Network.UnixSockets)
	for i, path := range paths {
		access := profile.Network.UnixSockets[path]
		if access != "allow" {
			return nil, fmt.Errorf("managed Codex app-server profile has invalid Unix-socket access %q", access)
		}
		if i > 0 {
			table.WriteByte(',')
		}
		table.WriteString(codexTOMLString(path))
		table.WriteByte('=')
		table.WriteString(codexTOMLString(access))
	}
	table.WriteString(`}}}`)

	overrides := []string{
		"default_permissions=" + codexTOMLString(name),
		"permissions." + name + "=" + table.String(),
	}
	if config.Features.NetworkProxy != nil {
		overrides = append(overrides, fmt.Sprintf("features.network_proxy=%t", *config.Features.NetworkProxy))
	}
	if config.Features.UseLegacyLandlock != nil {
		overrides = append(overrides, fmt.Sprintf("features.use_legacy_landlock=%t", *config.Features.UseLegacyLandlock))
	}
	return overrides, nil
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
