//go:build linux

package session

import (
	"os"
	"strings"
)

// Ubuntu 24.04+ ships an AppArmor policy whose whole job is to let `bwrap`
// create a user namespace while denying capabilities inside it: sandbox
// children are pivoted into its `unpriv_bwrap` child profile, which denies
// `capability sys_admin`. The outer tclaude wall is fine; the nested bwrap a
// stacked launch needs is not, because AppArmor's `capable` check applies to
// the profile regardless of who owns the namespace. Neither userns sysctl
// changes that.
const (
	appArmorEnabledPath       = "/sys/module/apparmor/parameters/enabled"
	appArmorBwrapPolicyPath   = "/etc/apparmor.d/bwrap-userns-restrict"
	appArmorBwrapDisabledPath = "/etc/apparmor.d/disable/bwrap-userns-restrict"
	appArmorBwrapComplainPath = "/etc/apparmor.d/force-complain/bwrap-userns-restrict"
)

// LikelyAppArmorNestedBwrapBlock reports whether this host most likely denies
// the nested bwrap that a stacked launch requires.
//
// It is a HINT, never a determination, and no caller may refuse or permit
// anything on it. The authoritative answer is the live nested round-trip the
// launch performs; this only decides whether a surface may say "likely" and
// point at the operator guide. Certainty would need `dmesg` or `aa-status`,
// which agentd cannot read: every check below is an unprivileged stat of a
// world-readable path.
func LikelyAppArmorNestedBwrapBlock() bool {
	return likelyAppArmorNestedBwrapBlock(
		appArmorEnabledPath,
		appArmorBwrapPolicyPath,
		appArmorBwrapDisabledPath,
		appArmorBwrapComplainPath,
	)
}

// likelyAppArmorNestedBwrapBlock takes its paths so a test can describe a host
// shape without one.
func likelyAppArmorNestedBwrapBlock(enabled, policy, disabled, complain string) bool {
	state, err := os.ReadFile(enabled)
	if err != nil || strings.TrimSpace(string(state)) != "Y" {
		return false
	}
	if _, err := os.Stat(policy); err != nil {
		return false
	}
	// An operator who unloaded the policy (`disable/`) or downgraded it to
	// logging (`force-complain/`) has already done the thing this hint would
	// tell them to do; claiming the block anyway would be the overclaim.
	// Lstat, because both entries are conventionally symlinks back into
	// /etc/apparmor.d and a dangling one still expresses the intent.
	for _, path := range []string{disabled, complain} {
		if _, err := os.Lstat(path); err == nil {
			return false
		}
	}
	return true
}
