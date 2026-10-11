package paf

import (
	"errors"
	"fmt"
	"io"

	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// WriteTranscript writes t as an agent's context file of the given scope.
// Inline media becomes content-addressed attachments, written before the
// context file because only one entry is open at a time. Empty text and
// summary blocks carry nothing and are skipped. Media with no data, URL or
// path, and entries left with no blocks, are dropped and recorded in
// t.Losses, so write losses.json after this call.
func WriteTranscript(w *Writer, key, scope string, t *transcript.Transcript) error {
	if t == nil {
		return errors.New("paf: nil transcript")
	}
	refs := map[*transcript.Media]string{}
	for i := range t.Entries {
		for j := range t.Entries[i].Blocks {
			b := &t.Entries[i].Blocks[j]
			for k := range b.Media {
				m := &b.Media[k]
				if len(m.Data) == 0 {
					continue
				}
				ref, err := w.AddAttachment(m.Data, m.MediaType)
				if err != nil {
					return err
				}
				refs[m] = ref
			}
		}
	}
	src := Source{Harness: t.Source.Harness, Format: t.Source.Format, ConvID: t.Source.ConvID, Model: t.Source.Model, Cwd: t.Source.Cwd, Title: t.Source.Title}
	if src.Harness == "" {
		return errors.New("paf: transcript source has no harness")
	}
	cw, closer, err := w.CreateContext(key, scope, src)
	if err != nil {
		return err
	}
	for i := range t.Entries {
		l, droppedMedia, ok := lineFromEntry(&t.Entries[i], refs)
		t.Losses.Add(transcript.LossMediaDropped, droppedMedia, "")
		if !ok {
			t.Losses.Add(transcript.LossUnknownRecord, 1, "empty entry")
			continue
		}
		if err := cw.Write(l); err != nil {
			_ = closer.Close()
			return err
		}
	}
	return closer.Close()
}

func lineFromEntry(e *transcript.Entry, refs map[*transcript.Media]string) (Line, int, bool) {
	dropped := 0
	l := Line{Type: LineEntry, ID: e.ID, Role: string(e.Role), Origin: string(e.Origin), Time: e.Time, Model: e.Model, Author: e.Author}
	for j := range e.Blocks {
		b := &e.Blocks[j]
		if (b.Kind == transcript.BlockText || b.Kind == transcript.BlockSummary) && b.Text == "" {
			continue
		}
		pb := Block{Kind: string(b.Kind), Text: b.Text, CallID: b.CallID, Name: b.Name, Input: b.Input, Freeform: b.Freeform, IsError: b.IsError}
		if b.Opaque != nil {
			pb.Opaque = &Opaque{Vendor: b.Opaque.Vendor, Data: b.Opaque.Data}
		}
		for k := range b.Media {
			m := &b.Media[k]
			pm := Media{MediaType: m.MediaType, Name: m.Name}
			switch {
			case refs[m] != "":
				pm.Attachment = refs[m]
			case m.URL != "":
				pm.URL = m.URL
			case m.Path != "":
				pm.Path = m.Path
			default:
				dropped++
				continue
			}
			pb.Media = append(pb.Media, pm)
		}
		if pb.Kind == BlockMedia && len(pb.Media) == 0 {
			continue
		}
		l.Blocks = append(l.Blocks, pb)
	}
	return l, dropped, len(l.Blocks) > 0
}

// ReadOptions bounds ReadTranscript.
type ReadOptions struct {
	// MaxMediaBytes bounds one attachment loaded inline.
	MaxMediaBytes int64
	// MaxTotalMediaBytes bounds the distinct attachment bytes loaded for
	// one transcript. Attachments are loaded once and shared by every
	// reference.
	MaxTotalMediaBytes int64
}

// mediaCache loads each attachment once, within a total budget.
type mediaCache struct {
	a     *Archive
	opts  ReadOptions
	data  map[string][]byte
	total int64
}

func (c *mediaCache) get(ref string) ([]byte, bool) {
	if b, ok := c.data[ref]; ok {
		return b, b != nil
	}
	p, ok := AttachmentPath(ref)
	if !ok {
		return nil, false
	}
	f := c.a.files[p]
	if !c.a.listed(p) || f == nil {
		c.data[ref] = nil
		return nil, false
	}
	size := int64(f.UncompressedSize64)
	if size > c.opts.MaxMediaBytes || c.total+size > c.opts.MaxTotalMediaBytes {
		c.data[ref] = nil
		return nil, false
	}
	b, err := c.a.ReadEntry(p, c.opts.MaxMediaBytes)
	if err != nil {
		b = nil
	}
	c.data[ref] = b
	c.total += int64(len(b))
	return b, b != nil
}

// ReadTranscript reads an agent's context file into the internal neutral
// transcript, loading attachments inline. An attachment over the per-item
// or total media budget is dropped and recorded as media_dropped. Content this package does not
// understand is never dropped silently: an unknown block kind becomes a
// text block marked as unsupported content, and unknown line types are
// counted in the loss report.
func ReadTranscript(a *Archive, key, scope string, opts ReadOptions) (*transcript.Transcript, error) {
	if opts.MaxMediaBytes <= 0 {
		opts.MaxMediaBytes = 64 << 20
	}
	if opts.MaxTotalMediaBytes <= 0 {
		opts.MaxTotalMediaBytes = 256 << 20
	}
	media := &mediaCache{a: a, opts: opts, data: map[string][]byte{}}
	cr, closer, err := a.Context(key, scope)
	if err != nil {
		return nil, err
	}
	defer func() { _ = closer.Close() }()
	h := cr.Header.Source
	t := transcript.New(transcript.Source{Harness: h.Harness, Format: h.Format, ConvID: h.ConvID, Model: h.Model, Cwd: h.Cwd, Title: h.Title})
	for {
		l, err := cr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch l.Type {
		case LineEntry:
		case LineBoundary:
			continue
		default:
			t.Losses.Add(transcript.LossUnknownRecord, 1, "paf line type "+l.Type)
			continue
		}
		e := transcript.Entry{ID: l.ID, Role: transcript.Role(l.Role), Origin: transcript.Origin(l.Origin), Time: l.Time, Model: l.Model, Author: l.Author}
		for _, b := range l.Blocks {
			e.Blocks = append(e.Blocks, blockToTranscript(media, b, t.Losses))
		}
		// Entries are appended as stored: the file's entry boundaries are
		// already the merged ones.
		t.Entries = append(t.Entries, e)
	}
	return t, nil
}

func blockToTranscript(media *mediaCache, b Block, losses *transcript.LossReport) transcript.Block {
	if !b.Known() {
		text := fmt.Sprintf("[unsupported content: %s block]", b.Kind)
		if b.Text != "" {
			text += "\n" + b.Text
		}
		losses.Add(transcript.LossUnknownRecord, 1, "paf block kind "+b.Kind)
		return transcript.Text(text)
	}
	tb := transcript.Block{Kind: transcript.BlockKind(b.Kind), Text: b.Text, CallID: b.CallID, Name: b.Name, Input: b.Input, Freeform: b.Freeform, IsError: b.IsError}
	if b.Opaque != nil {
		tb.Opaque = &transcript.Opaque{Vendor: b.Opaque.Vendor, Data: b.Opaque.Data}
	}
	for _, m := range b.Media {
		tm := transcript.Media{MediaType: m.MediaType, Name: m.Name, URL: m.URL, Path: m.Path}
		if m.Attachment != "" {
			data, ok := media.get(m.Attachment)
			if !ok {
				losses.Add(transcript.LossMediaDropped, 1, "")
				continue
			}
			tm.Data = data
		}
		tb.Media = append(tb.Media, tm)
	}
	return tb
}

// LossesFromReport converts a translation loss report to losses.json.
func LossesFromReport(r *transcript.LossReport) Losses {
	l := Losses{Items: []LossItem{}}
	if r == nil {
		return l
	}
	l = Losses{
		Source:                 r.Source,
		Target:                 r.Target,
		SourceEntries:          r.SourceEntries,
		CarriedEntries:         r.Carried,
		DroppedPercent:         r.DroppedPercent,
		KeptPercent:            r.KeptPercent,
		SourceEstimatedTokens:  r.SourceEstimatedTokens,
		CarriedEstimatedTokens: r.CarriedEstimatedTokens,
		ContextWindowTokens:    r.ContextWindowTokens,
		ContextWindowSource:    r.ContextWindowSource,
		BudgetTokens:           r.BudgetTokens,
		Fallback:               r.Fallback,
		LandingRung:            LandingRungFromFallback(r.Fallback),
		Items:                  []LossItem{},
	}
	for _, it := range r.Items {
		l.Items = append(l.Items, LossItem{Kind: string(it.Kind), Severity: string(it.Severity), Count: it.Count, Detail: it.Detail})
	}
	return l
}

// ReportFromLosses converts losses.json back to a loss report.
func ReportFromLosses(l *Losses) *transcript.LossReport {
	r := &transcript.LossReport{}
	if l == nil {
		return r
	}
	r.Source, r.Target = l.Source, l.Target
	r.SourceEntries, r.Carried = l.SourceEntries, l.CarriedEntries
	r.DroppedPercent, r.KeptPercent = l.DroppedPercent, l.KeptPercent
	r.SourceEstimatedTokens, r.CarriedEstimatedTokens = l.SourceEstimatedTokens, l.CarriedEstimatedTokens
	r.ContextWindowTokens, r.ContextWindowSource, r.BudgetTokens = l.ContextWindowTokens, l.ContextWindowSource, l.BudgetTokens
	r.Fallback = l.Fallback
	for _, it := range l.Items {
		r.Items = append(r.Items, transcript.Loss{Kind: transcript.LossKind(it.Kind), Severity: transcript.Severity(it.Severity), Count: it.Count, Detail: it.Detail})
	}
	return r
}
