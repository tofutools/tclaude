package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// An archive import has nothing a person can read in its body, so its popup shows
// what the caller's own preview would say: the handler runs on a copy of the
// request, stopped short of applying, with the answer kept for the popup. That is
// the review a client asking first is shown, so the two cannot drift apart.

// approvalReviewKey marks the copy, which has already reached the gate that is
// asking and answers only the popup, never the caller.
type approvalReviewKey struct{}

func reviewingForApproval(r *http.Request) bool {
	reviewing, _ := r.Context().Value(approvalReviewKey{}).(bool)
	return reviewing
}

// approvalReviewResponse keeps a handler's answer in memory.
type approvalReviewResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *approvalReviewResponse) Header() http.Header { return w.header }

func (w *approvalReviewResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *approvalReviewResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

// previewForApproval runs preview on a copy of r holding body, with nothing
// applied and no second popup. It returns the preview's JSON, or why there is none.
func previewForApproval(r *http.Request, body []byte, preview http.HandlerFunc) ([]byte, string) {
	ctx := context.WithValue(r.Context(), approvalReviewKey{}, true)
	// What the preview notes for the audit row is not what the request did.
	ctx = context.WithValue(ctx, auditResultContextKey{}, &auditResult{})
	copied := r.Clone(ctx)
	q := copied.URL.Query()
	q.Del("apply")
	copied.URL.RawQuery = q.Encode()
	copied.Header.Del("X-Tclaude-Ask-Human")
	copied.Body = io.NopCloser(bytes.NewReader(body))
	copied.ContentLength = int64(len(body))
	w := &approvalReviewResponse{header: http.Header{}}
	preview(w, copied)
	if w.status == http.StatusOK {
		return w.body.Bytes(), ""
	}
	var refused struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(w.body.Bytes(), &refused) != nil || refused.Error == "" {
		refused.Error = "the preview failed"
	}
	return nil, refused.Error
}

// approvalReviewText leads with what approving does and what must not be missed,
// because the file decides how long the rest is. A review past the preview limit
// says so before anything else.
func approvalReviewText(lead string, review agent.ImportReview) string {
	full := review.Full
	if len(full) > maxApprovalBodyPreview {
		lead = "This review is too long to show in full, so what comes past the cut is not listed. Decline unless you have read all of it.\n" + lead
		full = strings.ToValidUTF8(full[:maxApprovalBodyPreview], "") + "\n…[truncated]"
	}
	return lead + "\n\n" + review.Summary + "\n" + full
}

func agentImportApprovalFacts(r *http.Request, body []byte) string {
	preview, refused := previewForApproval(r, body, handleAgentBundleImport)
	if refused != "" {
		return "This agent file cannot be reviewed: " + guideText(refused, 300)
	}
	review, err := agent.AgentImportReview(preview)
	if err != nil {
		return "This agent file cannot be reviewed: " + guideText(err.Error(), 300)
	}
	lead := "Approving lets the agent read this review. Nothing is imported yet."
	if r.URL.Query().Get("apply") == "true" {
		lead = "Approving lands this agent here as a new one, with what is listed below, unless this node then refuses it."
		if r.URL.Query().Get("confirm_history_loss") == "true" {
			lead += " " + historyLossFacts
		}
	}
	return approvalReviewText(lead, review)
}

func groupImportApprovalFacts(r *http.Request, body []byte) string {
	preview, refused := previewForApproval(r, body, handleGroupImportInspect)
	if refused != "" {
		return "This group file cannot be reviewed: " + guideText(refused, 300)
	}
	review, err := agent.GroupImportReview(preview)
	if err != nil {
		return "This group file cannot be reviewed: " + guideText(err.Error(), 300)
	}
	lead := "Approving lets the agent read this review. Nothing is imported yet."
	apply := r.URL.Path == "/v1/groups/import"
	switch {
	case apply && review.Refused != "":
		lead = "This node refuses this file, so approving imports nothing: " + review.Refused
	case apply:
		lead = "Approving imports this group here, with what is listed below."
	case review.Refused != "":
		lead += " This node would refuse it: " + review.Refused
	}
	return approvalReviewText(lead, review)
}

// binaryApprovalBody reports a body a person cannot read. The prefix may stop
// inside a rune, which says nothing about the rest.
func binaryApprovalBody(prefix []byte) bool {
	if bytes.IndexByte(prefix, 0) >= 0 {
		return true
	}
	for range utf8.UTFMax - 1 {
		if utf8.Valid(prefix) {
			return false
		}
		prefix = prefix[:max(len(prefix)-1, 0)]
	}
	return !utf8.Valid(prefix)
}
