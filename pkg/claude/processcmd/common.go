package processcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
)

var (
	processNow          = time.Now
	defaultActorPattern = regexp.MustCompile(`[^A-Za-z0-9._@-]+`)
)

func openStore(root string, readOnly bool) (*store.FS, error) {
	if readOnly {
		if err := preflightStoreRoot(root); err != nil {
			return nil, err
		}
	}
	return store.NewFS(root)
}

func ensureRunVerifies(ctx context.Context, st store.Store, runID string, out io.Writer) error {
	report := processverify.StoreRun(ctx, st, runID)
	if !report.HasErrors() {
		return nil
	}
	renderReport(out, report)
	return fmt.Errorf("process run %q failed verification; refusing to advance", runID)
}

func printDiagnostics(w io.Writer, diagnostics model.Diagnostics) {
	for _, diag := range diagnostics {
		path := diag.Path
		if path == "" {
			path = "-"
		}
		fmt.Fprintf(w, "[%s] %s %s: %s\n", diag.Severity, diag.Code, path, diag.Message)
	}
}

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func sortedEdgeKeys(next model.Next) []string {
	keys := make([]string, 0, len(next))
	for key := range next {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func defaultActor() state.ActorRef {
	user := os.Getenv("USER")
	if strings.TrimSpace(user) == "" {
		user = "operator"
	}
	return state.ActorRef("human:" + defaultActorPattern.ReplaceAllString(user, "_"))
}

func logEntry(scope evidence.Scope, kind evidence.EntryKind, event state.Event, evidenceRef string, at time.Time) evidence.LogEntry {
	event.At = at
	if event.EvidenceRef == "" {
		event.EvidenceRef = evidenceRef
	}
	return evidence.LogEntry{
		SchemaVersion: evidence.LogEntrySchemaVersion,
		At:            at,
		Scope:         scope,
		Kind:          kind,
		Event:         &event,
		EvidenceRef:   evidenceRef,
	}
}

func nodeLogEntry(nodeID string, kind evidence.EntryKind, event state.Event, evidenceRef string, at time.Time) evidence.LogEntry {
	event.NodeID = nodeID
	return logEntry(evidence.Scope{Kind: evidence.ScopeNode, ID: nodeID}, kind, event, evidenceRef, at)
}

func runLogEntry(kind evidence.EntryKind, event state.Event, evidenceRef string, at time.Time) evidence.LogEntry {
	return logEntry(evidence.Scope{Kind: evidence.ScopeRun}, kind, event, evidenceRef, at)
}
