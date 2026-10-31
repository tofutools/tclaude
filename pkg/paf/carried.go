package paf

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
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

// CollectExtensions gathers agent key's unknown extension data from a, keeping
// only what the copy rule lets a tool that does not understand it keep, or
// returns nil when nothing is left. Whatever tclaude does next changes the file
// (a landing rebinds the conversation even when it lands natively), so
// extensions not declared copy: safe are dropped here and not named: the rule
// expects it. The tclaude id is always left out: this release writes its own,
// even for a version it cannot read.
//
// An id this package may not write, and an extension over budget, is dropped
// whole and named in dropped. Partial extension data could mean something else.
func CollectExtensions(a *Archive, key string) (c *CarriedExtensions, dropped []string, err error) {
	ids := map[string]bool{}
	for id, d := range a.Manifest.ExtensionsUsed {
		if id == ExtTclaude || a.UnderstandsExtension(id) || !d.CopySafe() {
			continue
		}
		if !ValidExtensionID(id) {
			dropped = append(dropped, id)
			continue
		}
		ids[id] = true
	}
	if len(ids) == 0 {
		return nil, dropped, nil
	}
	c = &CarriedExtensions{Decls: map[string]ExtensionDecl{}, Lines: map[string]Extensions{}}
	for id := range ids {
		c.Decls[id] = a.Manifest.ExtensionsUsed[id]
	}
	b := carryBudget{sizes: map[string]int{}, c: c, ids: ids, dropped: &dropped}
	pick := func(from Extensions) Extensions {
		out := Extensions{}
		for id, raw := range from {
			if ids[id] {
				out[id] = raw
				b.add(id, len(raw))
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
			b.dropID(id)
			continue
		}
		c.Entries = append(c.Entries, CarriedEntry{Path: e.Path, MediaType: e.MediaType, Data: data})
		b.add(id, len(data))
	}
	if a.HasContext(key, ScopeEffective) {
		if err := c.collectLines(a, key, &b); err != nil {
			return nil, nil, err
		}
	}
	if c.empty() {
		return nil, dropped, nil
	}
	return c, dropped, nil
}

// carryBudget holds CollectExtensions to MaxCarriedExtensionBytes as it reads,
// so no more than the budget and one entry is ever in memory.
type carryBudget struct {
	sizes   map[string]int
	total   int
	c       *CarriedExtensions
	ids     map[string]bool
	dropped *[]string
}

// add counts n bytes of id and drops the largest extension while over budget.
func (b *carryBudget) add(id string, n int) {
	b.sizes[id] += n
	b.total += n
	for b.total > MaxCarriedExtensionBytes {
		largest := ""
		for _, candidate := range slices.Sorted(maps.Keys(b.sizes)) {
			if largest == "" || b.sizes[candidate] > b.sizes[largest] {
				largest = candidate
			}
		}
		b.dropID(largest)
	}
}

func (b *carryBudget) dropID(id string) {
	b.total -= b.sizes[id]
	delete(b.sizes, id)
	delete(b.ids, id)
	b.c.drop(id)
	*b.dropped = append(*b.dropped, id)
}

func (c *CarriedExtensions) collectLines(a *Archive, key string, b *carryBudget) error {
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
			if !b.ids[id] {
				continue
			}
			if c.Lines[l.ID] == nil {
				c.Lines[l.ID] = Extensions{}
			}
			c.Lines[l.ID][id] = raw
			b.add(id, len(raw))
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

func (c *CarriedExtensions) empty() bool { return c == nil || len(c.Decls) == 0 }

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
