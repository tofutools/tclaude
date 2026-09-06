package harness

import (
	"fmt"
	"path/filepath"
	"strings"
)

// StateStoreIdentity is the launch-owned identity of the harness store that
// gives an external conversation reference its namespace. It is deliberately
// small enough to persist in an execution boundary without retaining the
// launch environment (which can contain secrets).
type StateStoreIdentity struct {
	Harness   string `json:"harness"`
	Namespace string `json:"namespace"`
	StateRoot string `json:"state_root"`
	Source    string `json:"source"`
}

// StateStoreLaunch is the already-composed launch input. Environment must be
// the environment the harness will inherit after launch precedence has been
// applied; adapters must not consult their own process environment.
type StateStoreLaunch struct {
	Environment       map[string]string
	ExplicitStateRoot string
}

// FrozenStateStoreContract is the portion of the persisted outer-layer
// contract needed to validate a captured identity during admission.
type FrozenStateStoreContract struct {
	HarnessName string
	StateRoot   string
}

// StateStoreIdentityAdapter captures and validates the per-execution namespace
// used by a harness's conversation store.
type StateStoreIdentityAdapter interface {
	CaptureStateStoreIdentity(StateStoreLaunch) (StateStoreIdentity, error)
	ValidateStateStoreIdentity(FrozenStateStoreContract, StateStoreIdentity) error
}

type pathStateStoreIdentity struct {
	harnessName  string
	overrideName string
	defaultDir   string
	explicitOnly bool
}

func (a pathStateStoreIdentity) CaptureStateStoreIdentity(launch StateStoreLaunch) (StateStoreIdentity, error) {
	root := strings.TrimSpace(launch.ExplicitStateRoot)
	source := "explicit launch allocation"
	if root == "" && !a.explicitOnly {
		if a.overrideName != "" {
			root = strings.TrimSpace(launch.Environment[a.overrideName])
			if root != "" {
				source = a.overrideName
			}
		}
		if root == "" {
			home := strings.TrimSpace(launch.Environment["HOME"])
			if home == "" {
				return StateStoreIdentity{}, fmt.Errorf("%s launch state store is unknown because HOME and %s are unset", a.harnessName, a.overrideName)
			}
			root = filepath.Join(home, a.defaultDir)
			source = "launch HOME"
		}
	}
	if root == "" {
		return StateStoreIdentity{}, fmt.Errorf("%s launch did not provide an authoritative state root", a.harnessName)
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return StateStoreIdentity{}, fmt.Errorf("%s launch state root %q is not absolute", a.harnessName, root)
	}
	return StateStoreIdentity{
		Harness: a.harnessName, Namespace: "host-path:" + root,
		StateRoot: root, Source: source,
	}, nil
}

func (a pathStateStoreIdentity) ValidateStateStoreIdentity(
	contract FrozenStateStoreContract,
	identity StateStoreIdentity,
) error {
	root := filepath.Clean(strings.TrimSpace(identity.StateRoot))
	if strings.TrimSpace(identity.Harness) != a.harnessName ||
		strings.TrimSpace(contract.HarnessName) != a.harnessName {
		return fmt.Errorf("state-store harness does not match frozen launch contract")
	}
	if root == "." || !filepath.IsAbs(root) ||
		filepath.Clean(strings.TrimSpace(contract.StateRoot)) != root {
		return fmt.Errorf("state-store root does not match frozen launch contract")
	}
	if strings.TrimSpace(identity.Namespace) != "host-path:"+root ||
		strings.TrimSpace(identity.Source) == "" {
		return fmt.Errorf("state-store namespace evidence is incomplete")
	}
	return nil
}

func claudeStateStoreIdentity() StateStoreIdentityAdapter {
	return pathStateStoreIdentity{harnessName: DefaultName, overrideName: "CLAUDE_CONFIG_DIR", defaultDir: ".claude"}
}

func codexStateStoreIdentity() StateStoreIdentityAdapter {
	return pathStateStoreIdentity{harnessName: CodexName, overrideName: "CODEX_HOME", defaultDir: ".codex"}
}

func copilotStateStoreIdentity() StateStoreIdentityAdapter {
	return pathStateStoreIdentity{harnessName: CopilotName, overrideName: CopilotHomeEnvVar, defaultDir: ".copilot"}
}

func openCodeStateStoreIdentity() StateStoreIdentityAdapter {
	return pathStateStoreIdentity{harnessName: OpenCodeName, explicitOnly: true}
}
