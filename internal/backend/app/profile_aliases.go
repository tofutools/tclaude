package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"strings"
	"unicode/utf8"
)

func normalizeConfigurationAliases(name string, values []string) ([]string, error) {
	var aliases []string
	seen := map[string]bool{}
	for _, value := range values {
		alias := strings.TrimSpace(value)
		if alias == "" || alias == name || seen[alias] || !utf8.ValidString(alias) || strings.ContainsAny(alias, "/\\") {
			return nil, ErrInvalid
		}
		for _, r := range alias {
			if r < 0x20 || r == 0x7f {
				return nil, ErrInvalid
			}
		}
		seen[alias] = true
		aliases = append(aliases, alias)
	}
	return aliases, nil
}

// ResolveConfigurationProfile accepts a human-facing name or alias and returns
// the current catalog selection; persisted references continue to use identity.
func (s *Service) ResolveConfigurationProfile(ctx context.Context, principal model.Principal, name string) (ConfigurationProfileResult, error) {
	profiles, err := s.ListConfigurationProfiles(ctx, principal)
	if err != nil {
		return ConfigurationProfileResult{}, err
	}
	name = strings.TrimSpace(name)
	var selected model.ConfigurationProfileID
	for _, profile := range profiles {
		matches := profile.Name == name
		for _, alias := range profile.Aliases {
			matches = matches || alias == name
		}
		if !matches {
			continue
		}
		if selected != "" {
			return ConfigurationProfileResult{}, ErrConflict
		}
		selected = profile.ID
	}
	if selected == "" {
		return ConfigurationProfileResult{}, ErrNotFound
	}
	return s.GetConfigurationProfile(ctx, principal, model.ConfigurationProfileRef{ProfileID: selected})
}
