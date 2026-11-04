package agentd

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// relaunchChanges reports, field by field, how the sandbox a resume or
// reincarnation applies differs from the one stored at the agent's last
// launch: the re-read rows of the relaunch policy table (sandbox profile
// assignment and contents, group environment, harness config posture). Each
// line names what changed, never a value, since environment values can be
// secrets. Nothing is reported when either side is missing or nothing
// changed.
//
// Agent-owned directory bindings are left out of the comparison: they are
// re-materialized on every relaunch and are not something anyone changed.
func relaunchChanges(previous, current *sandboxpolicy.Snapshot) []string {
	if previous == nil || current == nil {
		return nil
	}
	before := unmaterializedAgentDirectories(*previous)
	after := unmaterializedAgentDirectories(*current)
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	if b, a := appliedProfileNames(before.Applied), appliedProfileNames(after.Applied); b != a {
		add("sandbox profiles: %s → %s", orNone(b), orNone(a))
	} else if edited := editedProfiles(before.Applied, after.Applied); len(edited) > 0 {
		add("sandbox profile edited since the last launch: %s", strings.Join(edited, ", "))
	}
	if added, removed := filesystemDiff(before.Effective.Filesystem, after.Effective.Filesystem); added+removed > 0 {
		add("filesystem rules: %d added, %d removed", added, removed)
	}
	if line := environmentDiff("environment", withoutGeneratedEnvironment(before.Effective),
		withoutGeneratedEnvironment(after.Effective)); line != "" {
		out = append(out, line)
	}
	if line := environmentDiff("group environment", before.LaunchEnvironment, after.LaunchEnvironment); line != "" {
		out = append(out, line)
	}
	if before.Effective.NetworkAccess != after.Effective.NetworkAccess {
		add("network access: %s → %s", orDefault(string(before.Effective.NetworkAccess)), orDefault(string(after.Effective.NetworkAccess)))
	} else if !reflect.DeepEqual(before.Effective.Network, after.Effective.Network) {
		add("network rules changed")
	}
	// The remaining axes are named, not detailed: an edit to an included
	// profile does not move the applied profile's edit time, so each axis
	// is compared on its own.
	for _, axis := range []struct {
		name          string
		before, after any
	}{
		{"temporary filesystems", before.Effective.Tmpfs, after.Effective.Tmpfs},
		{"unix sockets", before.Effective.UnixSockets, after.Effective.UnixSockets},
		{"resource limits", before.Effective.ResourceLimits, after.Effective.ResourceLimits},
		{"pre-launch blocks", before.Effective.PreLaunch, after.Effective.PreLaunch},
		{"macOS mach register", before.Effective.DarwinAllowMachRegister, after.Effective.DarwinAllowMachRegister},
		{"macOS keychain write", before.Effective.DarwinDisableKeychainWrite, after.Effective.DarwinDisableKeychainWrite},
	} {
		if !sameAxis(axis.before, axis.after) {
			add("%s changed", axis.name)
		}
	}
	if before.Effective.FilesystemRoot != after.Effective.FilesystemRoot {
		add("filesystem root: %s → %s", orDefault(string(before.Effective.FilesystemRoot)), orDefault(string(after.Effective.FilesystemRoot)))
	}
	if before.Effective.HarnessConfig != after.Effective.HarnessConfig {
		add("harness config: %s → %s", orDefault(string(before.Effective.HarnessConfig)), orDefault(string(after.Effective.HarnessConfig)))
	}
	return out
}

// withoutGeneratedEnvironment drops the entries tclaude regenerates per
// launch (the Codex SSH workaround's command), which change on every
// relaunch without anyone changing anything.
func withoutGeneratedEnvironment(effective sandboxpolicy.EffectiveProfile) []sandboxpolicy.EnvironmentEntry {
	out := make([]sandboxpolicy.EnvironmentEntry, 0, len(effective.Environment))
	for _, entry := range effective.Environment {
		if effective.Provenance.Environment[entry.Name].Profile == codexSSHSourceName {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func appliedProfileNames(applied []sandboxpolicy.AppliedProfile) string {
	names := make([]string, 0, len(applied))
	for _, p := range applied {
		names = append(names, fmt.Sprintf("%s %q", p.Scope, p.Name))
	}
	return strings.Join(names, ", ")
}

// editedProfiles names the applied profiles whose stored edit time moved.
func editedProfiles(before, after []sandboxpolicy.AppliedProfile) []string {
	was := map[int64]sandboxpolicy.AppliedProfile{}
	for _, p := range before {
		was[p.ID] = p
	}
	var out []string
	for _, p := range after {
		if prior, ok := was[p.ID]; ok && !prior.UpdatedAt.Equal(p.UpdatedAt) {
			out = append(out, fmt.Sprintf("%q", p.Name))
		}
	}
	return out
}

func filesystemDiff(before, after []sandboxpolicy.FilesystemGrant) (added, removed int) {
	key := func(g sandboxpolicy.FilesystemGrant) string { return fmt.Sprintf("%#v", g) }
	was := map[string]bool{}
	for _, g := range before {
		was[key(g)] = true
	}
	now := map[string]bool{}
	for _, g := range after {
		now[key(g)] = true
		if !was[key(g)] {
			added++
		}
	}
	for k := range was {
		if !now[k] {
			removed++
		}
	}
	return added, removed
}

// environmentDiff names the variables added, removed or changed.
func environmentDiff(label string, before, after []sandboxpolicy.EnvironmentEntry) string {
	was := map[string]string{}
	for _, e := range before {
		was[e.Name] = e.Value
	}
	now := map[string]string{}
	for _, e := range after {
		now[e.Name] = e.Value
	}
	var added, removed, changed []string
	for name, value := range now {
		prior, ok := was[name]
		switch {
		case !ok:
			added = append(added, name)
		case prior != value:
			changed = append(changed, name)
		}
	}
	for name := range was {
		if _, ok := now[name]; !ok {
			removed = append(removed, name)
		}
	}
	var parts []string
	for _, part := range []struct {
		verb  string
		names []string
	}{{"added", added}, {"removed", removed}, {"changed", changed}} {
		if len(part.names) > 0 {
			sort.Strings(part.names)
			parts = append(parts, part.verb+" "+strings.Join(part.names, ", "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return label + ": " + strings.Join(parts, "; ")
}

// sameAxis compares one policy axis, treating an empty slice and a missing
// one as the same, since a stored snapshot's JSON drops empty axes.
func sameAxis(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Kind() == reflect.Slice && vb.Kind() == reflect.Slice && va.Len() == 0 && vb.Len() == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
