package model

import "fmt"

// Directory trust is an explicit native project trust-store choice. It grants
// no tclaude actions and does not change tool approval or confinement.
func ValidateDirectoryTrust(enabled bool, harness string) error {
	if !enabled {
		return nil
	}
	switch harness {
	case "claude", "codex", "copilot":
		return nil
	default:
		return fmt.Errorf("directory trust is unsupported by %s", harness)
	}
}

// DirectoryTrustAdmission retains caller-proven paths for deferred launches.
// It is internal admission evidence, never an authored configuration option.
type DirectoryTrustAdmission struct {
	AgentID            AgentID
	Harness            string
	RequestedTrust     bool
	RequestedDirectory string
	PhysicalDirectory  string
	Enabled            bool
	ProvenDirectories  []string
}

type WorkDirectoryTrust struct {
	RequestDigest string
	Nodes         map[WorkNodeID]DirectoryTrustAdmission
	// These optimistic admission fences are consumed inside the transaction.
	AgentRevisions     map[AgentID]Revision     `json:"-"`
	WorkspaceRevisions map[WorkspaceID]Revision `json:"-"`
}
