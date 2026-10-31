package paf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
)

// MaxCarriedExtensionBytes bounds the extension data one agent keeps between
// import and export. An extension over it is dropped whole.
const MaxCarriedExtensionBytes = 8 << 20

// CarriedExtensions is the data of extensions this package does not understand,
// taken from a file so a later export can write it back. See Collect.
type CarriedExtensions struct {
	Decls       map[string]ExtensionDecl `json:"decls"`
	Manifest    Extensions               `json:"manifest,omitempty"`
	Agent       Extensions               `json:"agent,omitempty"`
	Environment Extensions               `json:"environment,omitempty"`
	// Lines holds context entry extensions by entry id. An entry that no
	// longer exists under its id on export loses them.
	Lines map[string]Extensions `json:"lines,omitempty"`
	// Entries are the extensions' ext/<id>/ entries.
	Entries []CarriedEntry `json:"entries,omitempty"`
}

// CarriedEntry is one ext/<id>/ entry.
type CarriedEntry struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type,omitempty"`
	Data      []byte `json:"data"`
}

// CollectExtensions gathers agent key's unknown extension data from a, or
// returns nil when it has none. The tclaude id is always left out: this
// release writes its own, even for a version it cannot read.
//
// Extensions over budget are dropped whole, and named in dropped: partial
// extension data could mean something else.
func CollectExtensions(a *Archive, key string) (c *CarriedExtensions, dropped []string, err error) {
	ids := map[string]bool{}
	for id := range a.Manifest.ExtensionsUsed {
		if id != ExtTclaude && !a.UnderstandsExtension(id) {
			ids[id] = true
		}
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	c = &CarriedExtensions{Decls: map[string]ExtensionDecl{}, Lines: map[string]Extensions{}}
	for id := range ids {
		c.Decls[id] = a.Manifest.ExtensionsUsed[id]
	}
	pick := func(from Extensions) Extensions {
		out := Extensions{}
		for id, raw := range from {
			if ids[id] {
				out[id] = raw
			}
		}
		return out
	}
	c.Manifest = pick(a.Manifest.Extensions)
	if ag, ok := a.Agent(key); ok {
		c.Agent = pick(ag.Extensions)
	}
	if env, ok := a.Environment(key); ok {
		c.Environment = pick(env.Extensions)
	}
	for _, e := range a.Manifest.Entries {
		rest, ok := strings.CutPrefix(e.Path, ExtDir)
		if !ok {
			continue
		}
		id, _, _ := strings.Cut(rest, "/")
		if !ids[id] {
			continue
		}
		data, err := a.ReadEntry(e.Path, MaxCarriedExtensionBytes)
		if err != nil {
			dropped = append(dropped, id)
			c.drop(id)
			delete(ids, id)
			continue
		}
		c.Entries = append(c.Entries, CarriedEntry{Path: e.Path, MediaType: e.MediaType, Data: data})
	}
	if a.HasContext(key, ScopeEffective) {
		if err := c.collectLines(a, key, ids); err != nil {
			return nil, nil, err
		}
	}
	for id := range ids {
		if c.size() <= MaxCarriedExtensionBytes {
			break
		}
		dropped = append(dropped, id)
		c.drop(id)
	}
	if c.empty() {
		return nil, dropped, nil
	}
	return c, dropped, nil
}

func (c *CarriedExtensions) collectLines(a *Archive, key string, ids map[string]bool) error {
	cr, closer, err := a.Context(key, ScopeEffective)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()
	for {
		l, err := cr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read context extensions: %w", err)
		}
		if l.Type != LineEntry || l.ID == "" {
			continue
		}
		for id, raw := range l.Extensions {
			if !ids[id] {
				continue
			}
			if c.Lines[l.ID] == nil {
				c.Lines[l.ID] = Extensions{}
			}
			c.Lines[l.ID][id] = raw
		}
	}
}

// drop removes all of one extension, as the spec requires of a tool that
// cannot keep it.
func (c *CarriedExtensions) drop(id string) {
	delete(c.Decls, id)
	delete(c.Manifest, id)
	delete(c.Agent, id)
	delete(c.Environment, id)
	for key, line := range c.Lines {
		delete(line, id)
		if len(line) == 0 {
			delete(c.Lines, key)
		}
	}
	kept := c.Entries[:0]
	for _, e := range c.Entries {
		if !strings.HasPrefix(e.Path, ExtDir+id+"/") {
			kept = append(kept, e)
		}
	}
	c.Entries = kept
}

func (c *CarriedExtensions) size() int {
	raw, err := json.Marshal(c)
	if err != nil {
		return 0
	}
	return len(raw)
}

func (c *CarriedExtensions) empty() bool { return c == nil || len(c.Decls) == 0 }

// SafeOnly drops every extension not declared copy: safe, which is what the
// copy rule asks of a tool that changed the data they may describe. It changes
// c, and returns nil when nothing is left.
func (c *CarriedExtensions) SafeOnly() *CarriedExtensions {
	if c == nil {
		return nil
	}
	for id, d := range maps.Clone(c.Decls) {
		if !d.CopySafe() {
			c.drop(id)
		}
	}
	if c.empty() {
		return nil
	}
	return c
}

// WriteTo declares c's extensions on w and writes their entries. It merges the
// object data into m, ag and env, which the caller then writes, and makes
// WriteTranscript restore the entry extensions.
func (c *CarriedExtensions) WriteTo(w *Writer, m *Extensions, ag, env *Extensions) error {
	if c.empty() {
		return nil
	}
	for id, d := range c.Decls {
		if err := w.DeclareExtension(id, d, false); err != nil {
			return err
		}
	}
	merge := func(into *Extensions, from Extensions) {
		if len(from) == 0 {
			return
		}
		if *into == nil {
			*into = Extensions{}
		}
		maps.Copy(*into, from)
	}
	merge(m, c.Manifest)
	merge(ag, c.Agent)
	merge(env, c.Environment)
	for _, e := range c.Entries {
		if err := w.WriteFile(e.Path, e.MediaType, e.Data); err != nil {
			return err
		}
	}
	w.lineExt = c.Lines
	return nil
}
