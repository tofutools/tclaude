package pwf

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/paf"
)

// A workflow can travel in a PAF envelope (docs/paf.md, "The envelope") beside
// the agents that perform it or that carry it. Its files are the ones a .pwf
// holds, under workflows/<key>/, and one signature over the PAF manifest covers
// them. Everything a .pwf reader checks is checked here too.

// WriteEmbedded writes a bundle's workflow into an envelope under key.
func WriteEmbedded(w *paf.Writer, key string, b Bundle) error {
	c, err := bundleContent(b)
	if err != nil {
		return err
	}
	if err := w.AddWorkflow(key, b.Root, requiredFeatures(c.workflow)); err != nil {
		return err
	}
	limits := DefaultLimits()
	for _, f := range c.files {
		if int64(len(f.data)) > limits.MaxMetadataBytes {
			return fmt.Errorf("pwf: entry %q exceeds %d bytes", f.name, limits.MaxMetadataBytes)
		}
		if err := w.WriteFile(paf.WorkflowPath(key, f.name), f.mediaType, f.data); err != nil {
			return err
		}
	}
	return nil
}

// OpenEmbedded reads and fully verifies one workflow of an envelope, under the
// same bounds as a .pwf file. Its Signatures are the envelope's.
func OpenEmbedded(env *paf.Archive, key string) (*Archive, error) {
	var item *paf.WorkflowItem
	for i := range env.Manifest.Workflows {
		if env.Manifest.Workflows[i].Key == key {
			item = &env.Manifest.Workflows[i]
		}
	}
	if item == nil {
		return nil, fmt.Errorf("pwf: the file carries no workflow %q", key)
	}
	if unknown := unknownFeatures(item.Requires); len(unknown) > 0 {
		return nil, fmt.Errorf("pwf: workflow %q requires features this reader does not support: %s", key, strings.Join(unknown, ", "))
	}
	limits := DefaultLimits()
	prefix := paf.WorkflowPath(key, "")
	contents := map[string][]byte{}
	var total int64
	for _, e := range env.Manifest.Entries {
		rel, ok := strings.CutPrefix(e.Path, prefix)
		if !ok {
			continue
		}
		if total += e.Bytes; total > limits.MaxArchiveBytes || len(contents) >= limits.MaxEntries {
			return nil, fmt.Errorf("pwf: workflow %q exceeds what a workflow file may hold", key)
		}
		data, err := env.ReadEntry(e.Path, limits.MaxMetadataBytes)
		if err != nil {
			return nil, fmt.Errorf("pwf: workflow %q: %w", key, err)
		}
		contents[rel] = data
	}
	a := &Archive{
		ManifestRaw: env.ManifestRaw,
		Manifest:    Manifest{Format: Format, PwfVersion: Version, Kind: KindWorkflow, Producer: env.Manifest.Producer, Requires: item.Requires, Root: item.Root},
		Templates:   map[string]*Template{},
		Signatures:  env.Signatures,
	}
	if err := a.loadContents(contents); err != nil {
		return nil, err
	}
	return a, nil
}

// OpenEnvelope reads one workflow of an envelope; an empty key means the first.
// It returns the key it read.
func OpenEnvelope(env *paf.Archive, key string) (*Archive, string, error) {
	if len(env.Manifest.Workflows) == 0 {
		return nil, "", errors.New("pwf: the file carries no workflow")
	}
	if key == "" {
		key = env.Manifest.Workflows[0].Key
	}
	a, err := OpenEmbedded(env, key)
	return a, key, err
}
