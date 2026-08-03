//go:build darwin

package sandboxpolicy

// volumeFoldsNormalization is answered as a platform fact on macOS. Both native
// volume formats — APFS and HFS+ — compare names independently of Unicode
// normalization form, so an NFD and an NFC spelling of one name reach one
// directory regardless of whether the volume is case-sensitive. That is asserted
// against real hardware by the CaseAndNFCFollowFileIdentity assumption in
// pkg/claude/sandboxassumptions, which the macOS Seatbelt CI job hard-gates on
// an explicit PASS.
//
// Answering true for a foreign volume mounted on macOS (exFAT, SMB) is the safe
// direction for a guard: it can only produce a refusal, never a hole.
func volumeFoldsNormalization(string) (bool, error) { return true, nil }

// volumeFoldsNormalizationForCanonicalization matches its strict counterpart on
// darwin: the answer is a platform fact, so there is no weaker fallback to make.
func volumeFoldsNormalizationForCanonicalization(string) (bool, error) { return true, nil }
