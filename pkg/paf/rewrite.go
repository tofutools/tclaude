package paf

import (
	"errors"
	"io"
	"slices"
	"strings"
)

// WithPrivacy writes a copy of a with privacy.json replaced by p. Every
// other entry is copied byte for byte, so entry hashes and the content
// digest are unchanged; the manifest is new and signed by keys. The old
// signatures do not carry over.
func WithPrivacy(a *Archive, out io.Writer, p Privacy, limits Limits, producer Producer, keys ...SigningKey) error {
	m := a.Manifest
	w := NewWriter(out, m.Kind, limits)
	w.SetCreatedAt(m.CreatedAt)
	for _, f := range m.Requires {
		w.Require(f)
	}
	for id, d := range m.ExtensionsUsed {
		w.extUsed[id] = d
	}
	w.extReq = append(w.extReq, m.ExtensionsRequired...)
	w.manExt = m.Extensions
	w.workflows = slices.Clone(m.Workflows)
	w.links = slices.Clone(m.Links)
	for _, e := range m.Entries {
		if e.Path == PrivacyPath {
			continue
		}
		if err := copyEntry(a, w, e); err != nil {
			return err
		}
	}
	w.agents = append(w.agents, m.Agents...)
	for _, key := range m.Agents {
		if ag, ok := a.Agent(key); ok && ag.Native != nil {
			w.natives = append(w.natives, ag.Native.Path)
		}
	}
	if g := a.Group(); g != nil {
		for _, mem := range g.Members {
			w.members = append(w.members, mem.Agent)
		}
	}
	if err := w.WriteJSON(PrivacyPath, p); err != nil {
		return err
	}
	_, err := w.Close(producer, keys...)
	return err
}

func copyEntry(a *Archive, w *Writer, e Entry) error {
	if strings.HasPrefix(e.Path, SignaturesDir) || e.Path == ManifestPath {
		return errors.New("paf: manifest entry lists " + e.Path)
	}
	rc, err := a.OpenEntry(e.Path)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	ew, err := w.Create(e.Path, e.MediaType)
	if err != nil {
		return err
	}
	if _, err := io.Copy(ew, rc); err != nil {
		return err
	}
	return ew.Close()
}
