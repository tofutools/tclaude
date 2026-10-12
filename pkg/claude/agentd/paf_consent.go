package agentd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
)

// pafConsentBy is who a consent record names: the human operator, the only
// caller this route accepts.
const pafConsentBy = "operator"

// handlePAFConsent records a human's consent on an uploaded PAF and returns
// the re-signed copy.
//
//	POST /v1/paf/consent?audience=self|recipients|board|public[&recipient=<id>...][&board=<name>]
//
// Consent is the human confirmation sharing and publishing require, so the
// route is human-only: an agent can never consent on its own authority.
// The content entries are copied byte for byte; only privacy.json, the
// manifest and the signature change.
func handlePAFConsent(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "consent to sharing a PAF") {
		return
	}
	q := r.URL.Query()
	req := privacy.ConsentRequest{By: pafConsentBy, Audience: q.Get("audience"), Recipients: q["recipient"], Board: q.Get("board")}
	limits := paf.DefaultLimits()
	in, err := os.CreateTemp("", ".paf-consent-in-")
	if err != nil {
		writeError(w, 500, "archive", err.Error())
		return
	}
	defer func() { _ = in.Close(); _ = os.Remove(in.Name()) }()
	n, err := io.Copy(in, http.MaxBytesReader(w, r.Body, limits.MaxArchiveBytes+1))
	if err != nil || n > limits.MaxArchiveBytes {
		writeError(w, 400, "archive", fmt.Sprintf("archive exceeds or cannot be read within %d bytes", limits.MaxArchiveBytes))
		return
	}
	// Consent rewrites nothing it does not understand: every content entry
	// is copied as is, so unknown features need not be refused.
	a, err := paf.Open(in, n, paf.Options{Limits: limits, Purpose: paf.PurposeInspect})
	if err != nil {
		writeError(w, 400, "archive", err.Error())
		return
	}
	var keys []paf.SigningKey
	// Signed only when this node already has a federation identity, as a
	// local export is; consenting never creates one.
	if id, err := proto.LoadIdentity(FederationKeyPath()); err == nil {
		keys = []paf.SigningKey{{Key: id.Priv, KeyID: id.ID(), Signer: &paf.Signer{Kind: "instance"}}}
	}
	out, err := os.CreateTemp("", ".paf-consent-out-")
	if err != nil {
		writeError(w, 500, "archive", err.Error())
		return
	}
	defer func() { _ = out.Close(); _ = os.Remove(out.Name()) }()
	if _, err := privacy.AddConsent(a, out, req, limits, paf.Producer{Name: "tclaude", Version: buildversion.AppVersion()}, keys...); err != nil {
		writeError(w, 400, "consent", err.Error())
		return
	}
	size, err := out.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = out.Seek(0, io.SeekStart)
	}
	if err != nil {
		writeError(w, 500, "archive", err.Error())
		return
	}
	w.Header().Set("Content-Type", paf.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, out)
}
