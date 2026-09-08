package sandboxpolicy

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

const BundleFormat = "tclaude-sandbox-profiles"
const BundleMaxBytes = 16 << 20

// Bundle transfers authored values only. Original references identify the graph;
// they confer no authority and are remapped to independent identities on import.
type Bundle struct {
	Format  string                  `json:"format"`
	Version int                     `json:"version"`
	Root    model.SandboxProfileRef `json:"root"`
	Entries []BundleEntry           `json:"entries"`
}
type BundleEntry struct {
	Ref    model.SandboxProfileRef `json:"ref"`
	Name   string                  `json:"name"`
	Policy model.SandboxPolicy     `json:"policy"`
}
type bundleReader map[model.SandboxProfileRevisionID]BundleEntry

func (r bundleReader) ReadSandboxRevision(_ context.Context, ref model.SandboxProfileRef) (model.SandboxPolicy, error) {
	entry, ok := r[ref.RevisionID]
	if !ok || entry.Ref != ref {
		return model.SandboxPolicy{}, invalidClosure("bundle is missing an exact included revision")
	}
	return entry.Policy, nil
}

// InspectBundle returns a detached, topologically ordered graph. It never reads
// host paths or fills missing dependencies from the destination catalog.
func InspectBundle(ctx context.Context, bundle Bundle) (Bundle, error) {
	if bundle.Format != BundleFormat || bundle.Version != 1 || len(bundle.Entries) == 0 || len(bundle.Entries) > 128 {
		return Bundle{}, invalidClosure("unsupported sandbox bundle format/version or entry count")
	}
	reader := bundleReader{}
	for _, entry := range bundle.Entries {
		if entry.Name == "" || entry.Name != strings.TrimSpace(entry.Name) || len(entry.Name) > 200 || !utf8.ValidString(entry.Name) || strings.ContainsRune(entry.Name, 0) {
			return Bundle{}, invalidClosure("invalid sandbox bundle name")
		}
		if _, ok := reader[entry.Ref.RevisionID]; ok {
			return Bundle{}, invalidClosure("duplicate sandbox bundle revision")
		}
		// Validate before JSON encoding so invalid UTF-8 cannot be silently replaced.
		if err := Validate(entry.Policy); err != nil {
			return Bundle{}, invalidClosure(err.Error())
		}
		reader[entry.Ref.RevisionID] = entry
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return Bundle{}, err
	}
	if len(data) > BundleMaxBytes {
		return Bundle{}, invalidClosure("sandbox bundle exceeds 16 MiB")
	}
	closure, err := Resolve(ctx, bundle.Root, reader)
	if err != nil {
		return Bundle{}, err
	}
	if len(closure.Entries) != len(reader) {
		return Bundle{}, invalidClosure("sandbox bundle contains unreachable revisions")
	}
	out := Bundle{Format: BundleFormat, Version: 1, Root: closure.Root, Entries: make([]BundleEntry, 0, len(closure.Entries))}
	for _, entry := range closure.Entries {
		out.Entries = append(out.Entries, BundleEntry{entry.Ref, reader[entry.Ref.RevisionID].Name, entry.Policy})
	}
	return out, nil
}

func (r bundleReader) CurrentSandboxRef(_ context.Context, id model.SandboxProfileID) (model.SandboxProfileRef, error) {
	var ref model.SandboxProfileRef
	for _, entry := range r {
		if entry.Ref.ProfileID == id {
			if ref.ProfileID != "" {
				return ref, invalidClosure("ambiguous included profile")
			}
			ref = entry.Ref
		}
	}
	if ref.ProfileID == "" {
		return ref, invalidClosure("missing included profile")
	}
	return ref, nil
}
