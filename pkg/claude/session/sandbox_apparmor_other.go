//go:build !linux

package session

// LikelyAppArmorNestedBwrapBlock is a Linux-only heuristic: AppArmor policy is
// not what stops a nested sandbox anywhere else. Elsewhere a stacked launch
// fails for its own platform reason and says so, so this hint stays silent
// rather than volunteering an explanation that cannot apply.
func LikelyAppArmorNestedBwrapBlock() bool { return false }
