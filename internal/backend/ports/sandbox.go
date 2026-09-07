package ports

import (
	"context"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// SandboxPathObservation is a read-only preview, never a mount capability or
// launch receipt. Available means the path was inspectable at this observation;
// admission/release must separately bind and recheck its exact host identity.
type SandboxPathObservation struct {
	Index         int
	CanonicalPath string
	Kind          string
	State         string
	Detail        string
}

type SandboxPathInspector interface {
	ResolveSandboxHostPath(context.Context, string) (string, error)
	InspectSandboxPaths(context.Context, []model.SandboxFilesystemRule) ([]SandboxPathObservation, error)
}
