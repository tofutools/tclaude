package ports

import "context"

// DirectoryWriteProof inspects markers created by the caller inside its own
// sandbox. The application owns challenge identity, expiry and consumption;
// this host boundary never creates a proof on the caller's behalf.
type DirectoryWriteProof interface {
	ResolveProofDirectories(context.Context, []string) ([]string, error)
	VerifyProofMarkers(context.Context, []string, string) error
	ReassertProofDirectories(context.Context, []string) error
	RemoveProofMarkers(context.Context, []string, string) error
}

// DirectoryTrustWorktree identifies v1's default sibling layout and the exact
// Git metadata directory whose caller-side proof accompanies that exception.
type DirectoryTrustWorktree interface {
	DefaultSiblingTrustDirectories(context.Context, string) ([]string, error)
}

const DirectoryWriteProofPrefix = ".tclaude-write-proof-"
