package agentd

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
)

// handlePAFPublish prepares, or with the reviewed digest produces, a PAF
// for public sharing.
//
//	POST /v1/paf/publish[?content_digest=<hex>][&stub_tool_results=true&keep_turns=N][&drop_media=true][&drop_reasoning=true]
//
// The body is a one-agent PAF. Publishing always masks flagged
// credentials, replaces paths with placeholders, strips identity and drops
// the native blob and extension data; the query adds optional redactions.
// Without content_digest the route returns the redaction report and the
// summary of the redacted copy: what the human reviews. With the digest of
// that copy it records the publisher's consent for a public audience and
// returns the copy signed by this node's pseudonymous publisher key, never
// by its federation key. Human-only: an agent can never publish.
func handlePAFPublish(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "publish a PAF") {
		return
	}
	q := r.URL.Query()
	opts := privacy.PublishOptions{StubToolResults: q.Get("stub_tool_results") == "true", DropMedia: q.Get("drop_media") == "true",
		DropReasoning: q.Get("drop_reasoning") == "true", Native: decodeNativeHistory, Limits: paf.DefaultLimits()}
	if v := q.Get("keep_turns"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, 400, "keep_turns", "keep_turns must be a non-negative number")
			return
		}
		opts.KeepTurns = n
	}
	limits := opts.Limits
	in, err := os.CreateTemp("", ".paf-publish-in-")
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
	a, err := paf.Open(in, n, paf.Options{Limits: limits})
	if err != nil {
		writeError(w, 400, "archive", err.Error())
		return
	}
	scratch, err := os.CreateTemp("", ".paf-publish-scratch-")
	if err != nil {
		writeError(w, 500, "archive", err.Error())
		return
	}
	defer func() { _ = scratch.Close(); _ = os.Remove(scratch.Name()) }()
	producer := paf.Producer{Name: "tclaude", Version: buildversion.AppVersion()}
	p, err := privacy.PreparePublication(a, scratch, opts, producer, pafContextRecordLimit())
	if err != nil {
		writeError(w, 422, "publish", err.Error())
		return
	}
	want := q.Get("content_digest")
	if want == "" {
		writeJSON(w, 200, map[string]any{"report": p.Report, "summary": p.Summary})
		return
	}
	if want != p.Summary.Digest {
		writeError(w, 409, "consent", "the published copy would differ from the one reviewed (content digest "+p.Summary.Digest+", reviewed "+want+")")
		return
	}
	key, err := publisherKey()
	if err != nil {
		writeError(w, 500, "publisher_key", err.Error())
		return
	}
	out, err := os.CreateTemp("", ".paf-publish-out-")
	if err != nil {
		writeError(w, 500, "archive", err.Error())
		return
	}
	defer func() { _ = out.Close(); _ = os.Remove(out.Name()) }()
	if err := privacy.FinishPublication(p, out, producer, key, limits); err != nil {
		writeError(w, 422, "publish", err.Error())
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
	recordFederationAudit("paf.publish", "", "", "", "content_digest="+p.Summary.Digest+" key="+key.KeyID, 200)
	w.Header().Set("Content-Type", paf.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, out)
}

// decodeNativeHistory reads a native history through its harness, so a
// history with no neutral context survives the drop of the native blob.
func decodeNativeHistory(n paf.Native, r io.Reader) (*transcript.Transcript, error) {
	if n.Harness == "" {
		return nil, fmt.Errorf("native %s history names no harness", n.Format)
	}
	h, err := harness.Resolve(n.Harness)
	if err != nil {
		return nil, err
	}
	if h.Transcript == nil {
		return nil, fmt.Errorf("tclaude cannot read %s native history", h.Name)
	}
	return h.Transcript.Decode(r, n.SourceConvID)
}

var publisherKeyMu sync.Mutex

// publisherKeyPath holds this node's pseudonymous publisher key. It is
// separate from the federation identity, so a published PAF cannot be tied
// to the node, while publications from this node share one key.
func publisherKeyPath() string {
	return filepath.Join(config.DataDir(), "paf", "publisher.key")
}

// publisherKey loads the publisher key, creating it on first use.
func publisherKey() (paf.SigningKey, error) {
	publisherKeyMu.Lock()
	defer publisherKeyMu.Unlock()
	path := publisherKeyPath()
	var priv ed25519.PrivateKey
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		block, _ := pem.Decode(raw)
		if block == nil || block.Type != "TCLAUDE PAF PUBLISHER KEY" || len(block.Bytes) != ed25519.SeedSize {
			return paf.SigningKey{}, errors.New("unreadable publisher key " + path)
		}
		priv = ed25519.NewKeyFromSeed(block.Bytes)
	case errors.Is(err, os.ErrNotExist):
		if _, priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
			return paf.SigningKey{}, err
		}
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return paf.SigningKey{}, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return paf.SigningKey{}, err
		}
		err = pem.Encode(f, &pem.Block{Type: "TCLAUDE PAF PUBLISHER KEY", Bytes: priv.Seed()})
		if syncErr := f.Sync(); err == nil {
			err = syncErr
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
			return paf.SigningKey{}, err
		}
	default:
		return paf.SigningKey{}, err
	}
	sum := sha256.Sum256(priv.Public().(ed25519.PublicKey))
	return paf.SigningKey{Key: priv, KeyID: "pub_" + hex.EncodeToString(sum[:8]), Signer: &paf.Signer{Kind: "publisher"}}, nil
}
