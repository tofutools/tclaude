//go:build !darwin

package sandboxpolicy

import "golang.org/x/text/unicode/norm"

// volumeFoldsNormalization probes whether the filesystem hosting dir treats
// NFC and NFD spellings of a name as one name.
//
// Unlike macOS there is no platform-wide answer here: Linux filesystems are
// byte-oriented and normalization-sensitive as a rule, but the rule is not
// universal across mounts. So this runs the same empirical respelling probe the
// case check uses. A path with no component that changes under normalization
// cannot answer the question and returns errSpellingProbeUnavailable, which
// guards read as a refusal.
//
// This branch is only ever reached for paths carrying non-ASCII names: a pure
// ASCII spelling is identical in NFC and NFD, so it can never produce a
// normalization-only difference for a guard to resolve.
func volumeFoldsNormalization(dir string) (bool, error) {
	return volumeFoldsSpelling(dir, flipNormalization)
}

// flipNormalization returns the opposite normalization form of name, or name
// unchanged when the two forms coincide (every pure-ASCII name).
func flipNormalization(name string) string {
	if nfc := norm.NFC.String(name); nfc != name {
		return nfc
	}
	if nfd := norm.NFD.String(name); nfd != name {
		return nfd
	}
	return name
}
