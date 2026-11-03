package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// validateGitProxy is Validate's agent.git_proxy part: what the dashboard's
// Git & forges form must not save. ResolvedGitProxy stays lenient and keeps
// dropping a bad entry with a warning, so a hand-edited file behaves as it
// always has; a human editing in the form is told instead.
func validateGitProxy(c *Config) []string {
	if c == nil || c.Agent == nil || c.Agent.GitProxy == nil {
		return nil
	}
	gp := c.Agent.GitProxy
	var errs []string

	for _, raw := range gp.AllowedRemotes {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			continue // dropped on load, and harmless
		}
		lower := strings.ToLower(pattern)
		if strings.Contains(lower, "://") || strings.HasPrefix(lower, "git@") || strings.Contains(pattern, ":") {
			errs = append(errs, fmt.Sprintf(
				"agent.git_proxy.allowed_remotes: %q must be a host/owner/repo pattern such as github.com/acme/app, not a URL", pattern))
			continue
		}
		for _, seg := range strings.Split(strings.Trim(pattern, "/"), "/") {
			if seg == "" {
				errs = append(errs, fmt.Sprintf("agent.git_proxy.allowed_remotes: %q has an empty segment", pattern))
				break
			}
		}
	}

	if gp.ProtectedRefs != nil {
		for _, raw := range *gp.ProtectedRefs {
			if strings.TrimSpace(raw) == "" {
				errs = append(errs, "agent.git_proxy.protected_refs: an entry is blank; remove it, or leave the list empty to turn the protection off")
				break
			}
		}
	}

	hosts := make([]string, 0, len(gp.GitLabHosts))
	for host := range gp.GitLabHosts {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	bases := map[string]string{}
	reported := map[string]bool{}
	for _, rawHost := range hosts {
		host := strings.ToLower(strings.TrimSpace(rawHost))
		base := strings.TrimRight(strings.TrimSpace(gp.GitLabHosts[rawHost]), "/")
		if host == "" {
			errs = append(errs, "agent.git_proxy.gitlab_hosts: a host is empty")
			continue
		}
		if strings.ContainsAny(host, "/:@ ") {
			errs = append(errs, fmt.Sprintf(
				"agent.git_proxy.gitlab_hosts: %q must be a host name alone, as it appears in the remote, with no scheme, port or path", rawHost))
			continue
		}
		if msg := gitLabBaseProblem(base); msg != "" {
			errs = append(errs, fmt.Sprintf("agent.git_proxy.gitlab_hosts[%q]: %s", host, msg))
			continue
		}
		if prev, ok := bases[host]; ok && !strings.EqualFold(prev, base) && !reported[host] {
			reported[host] = true
			errs = append(errs, fmt.Sprintf("agent.git_proxy.gitlab_hosts: %q is listed twice with different bases", host))
		}
		bases[host] = base
	}
	return errs
}

// gitLabBaseProblem says what is wrong with a gitlab_hosts base, or "" when it
// is an https origin. It is the rule normalizeGitLabHosts applies on load.
func gitLabBaseProblem(base string) string {
	const want = "must be an https origin such as https://gitlab.example.com:8929"
	if base == "" {
		return "the base URL is empty; it " + want
	}
	u, err := url.Parse(base)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return fmt.Sprintf("%q %s", base, want)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Sprintf("%q %s, with no path, query or user", base, want)
	}
	return ""
}
