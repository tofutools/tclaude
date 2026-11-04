package agentd

import (
	"encoding/json"
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A saved page's marks and live patches (tcl-01arix) are kept with its
// group_pages row, so a page that closes or outlives a daemon restart
// reopens with the operator's marks in `page marks` and its patches
// re-applied. The writes follow pageCollab's in-memory state and are made
// under pageCollab.mu, so a write cannot overtake a newer one. Every write
// names the page and saves nothing for a page with no saved row (a private
// page), so the callers need not know which kind it is. Best-effort, like the
// row itself: the live page works either way.

// saveMarkBatchLocked saves a batch the operator sent. Caller holds pageCollab.mu.
func saveMarkBatchLocked(id string, b pageMarkBatch) {
	raw, err := json.Marshal(b)
	if err == nil {
		err = db.AddGroupPageMarks(id, b.Seq, string(raw), pageMarksMaxBatches)
	}
	if err != nil {
		slog.Warn("pages: could not save a page's marks", "page", id, "error", err)
	}
}

// clearSavedMarksLocked drops a page's saved marks. Caller holds pageCollab.mu.
func clearSavedMarksLocked(id string) {
	if err := db.ClearGroupPageMarks(id); err != nil {
		slog.Warn("pages: could not drop a page's saved marks", "page", id, "error", err)
	}
}

// savePatchesLocked saves a page's patch journal as it is now. Caller holds
// pageCollab.mu.
func savePatchesLocked(id string, s *pageCollabState) {
	patches := s.patches
	if patches == nil {
		patches = []pagePatch{}
	}
	raw, err := json.Marshal(patches)
	if err == nil {
		err = db.SaveGroupPagePatches(id, s.seq, s.patchSeq, string(raw))
	}
	if err != nil {
		slog.Warn("pages: could not save a page's live patches", "page", id, "error", err)
	}
}

// clearSavedCollabLocked drops everything a page kept: shown again from
// another kind of source, it starts again, as its memory does. Caller holds
// pageCollab.mu, or the registry's lock that orders it.
func clearSavedCollabLocked(id string) {
	if err := db.ClearGroupPageCollab(id); err != nil {
		slog.Warn("pages: could not drop a page's saved marks and patches", "page", id, "error", err)
	}
}

// loadSavedCollab reads what a saved page kept, nil when nothing. Call it
// without the page locks; restoreSavedCollabLocked installs it.
func loadSavedCollab(id string) *db.GroupPageCollab {
	c, err := db.LoadGroupPageCollab(id)
	if err != nil {
		slog.Warn("pages: could not read a page's saved marks and patches", "page", id, "error", err)
		return nil
	}
	return c
}

// restoreSavedCollabLocked gives a page reopening under its saved id the
// marks and patches it kept, unless it still has state here (it never
// closed). Caller holds pageCollab.mu.
func restoreSavedCollabLocked(id string, c *db.GroupPageCollab) {
	if c == nil || pageCollab.byID[id] != nil {
		return
	}
	s := pageCollabLocked(id)
	s.seq, s.patchSeq = c.MarkSeq, c.PatchSeq
	for _, raw := range c.Batches {
		var b pageMarkBatch
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			slog.Warn("pages: dropping a saved mark batch that does not read", "page", id, "error", err)
			continue
		}
		s.batches = append(s.batches, b)
		s.seq = max(s.seq, b.Seq)
	}
	var patches []pagePatch
	if err := json.Unmarshal([]byte(c.Patches), &patches); err != nil {
		slog.Warn("pages: dropping saved live patches that do not read", "page", id, "error", err)
		patches = nil
	}
	for _, p := range patches {
		s.patches = append(s.patches, p)
		s.patchBytes += p.size()
		s.patchSeq = max(s.patchSeq, p.N)
	}
}

// restoreSavedCollab is restoreSavedCollabLocked taking the lock.
func restoreSavedCollab(id string, c *db.GroupPageCollab) {
	pageCollab.mu.Lock()
	restoreSavedCollabLocked(id, c)
	pageCollab.mu.Unlock()
}
