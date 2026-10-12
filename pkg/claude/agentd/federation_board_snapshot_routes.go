package agentd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
	"github.com/tofutools/tclaude/pkg/paf/seal"
)

// Agent snapshots on Boards. A snapshot is shared as a PAF sealed to named
// board members (recipient-only), split into board blobs, with a hard
// expiry the hub enforces. Every route is human-only, like the other board
// item routes: publishing needs a human's confirmation of the reviewed
// content digest, and importing needs a preview token.

const boardSnapshotKind = "agent-snapshot"

func registerBoardSnapshotRoutes(mux *http.ServeMux, prefix string, dashboard bool) {
	for pattern, op := range map[string]string{
		"GET /{board}/snapshots":                           "list",
		"POST /{board}/snapshots":                          "share",
		"POST /{board}/snapshots/{item}/{version}/fetch":   "fetch",
		"POST /{board}/snapshots/{item}/{version}/preview": "preview",
		"POST /{board}/snapshots/{item}/{version}/import":  "import",
		"DELETE /{board}/snapshots/{item}/{version}":       "delete",
	} {
		method, tail, _ := strings.Cut(pattern, " ")
		handler := boardSnapshotRoute(op)
		if dashboard {
			handler = dashboardFederationRoute(handler)
		}
		mux.HandleFunc(method+" "+prefix+tail, handler)
	}
}

func boardSnapshotRoute(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHuman(w, r, "share, fetch or import board snapshots") {
			return
		}
		ref := boardItemRef{r.PathValue("board"), r.PathValue("item"), r.PathValue("version")}
		if !proto.ValidStreamID(ref.Board) || (ref.Item != "" && !proto.ValidStreamID(ref.Item)) || (ref.Version != "" && !proto.ValidStreamID(ref.Version)) {
			writeError(w, 400, "board_item", "invalid board, item or version id")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		a, e := newBoardItemAccess(ctx)
		if e != nil {
			writeError(w, 409, "board_unavailable", e.Error())
			return
		}
		sweepBoardSnapshotSpool(time.Now())
		var err error
		switch op {
		case "list":
			err = a.snapshotList(w, ref.Board, r.URL.Query().Get("cursor"))
		case "share":
			err = a.snapshotShare(w, r, ref.Board)
		case "fetch":
			err = a.snapshotFetch(w, ref)
		case "preview", "import":
			err = a.snapshotImport(w, r, op, ref)
		case "delete":
			// The local copy goes first and always: a recipient can
			// discard what it fetched even when only the publisher or an
			// owner may delete the snapshot from the board.
			removeBoardSnapshotSpool(ref)
			if r.URL.Query().Get("local") == "true" {
				writeJSON(w, 200, map[string]any{"deleted": false, "local_removed": true})
				break
			}
			if _, err = a.call("items.delete", map[string]any{"board": ref.Board, "item": ref.Item, "version_id": ref.Version}); err == nil {
				recordFederationAudit("boards.snapshot.delete", "", "", ref.Board, "item="+ref.Item+" version="+ref.Version, 200)
				writeJSON(w, 200, map[string]any{"deleted": true, "local_removed": true})
			}
		}
		if err != nil {
			var be *boardHTTPError
			if errors.As(err, &be) {
				writeError(w, be.Status, be.Code, be.Message)
			} else {
				writeError(w, 502, "board_snapshot", err.Error())
			}
		}
	}
}

// snapshotCapable refuses hubs that do not enforce the snapshot contract
// (hard expiry, recipient binding, sub-quota).
func (a *boardItemAccess) snapshotCapable() error {
	raw, err := a.call("boards.capabilities", map[string]any{})
	if err == nil {
		var caps struct {
			Capabilities []string `json:"capabilities"`
		}
		if err = json.Unmarshal(raw, &caps); err == nil && slices.Contains(caps.Capabilities, proto.BoardSnapshotsCapability) {
			return nil
		}
	}
	return &boardHTTPError{409, "hub_unsupported", "this hub does not support board snapshots (" + proto.BoardSnapshotsCapability + "); nothing was published"}
}

func (a *boardItemAccess) boardMembers(board string) ([]boardMember, error) {
	var members []boardMember
	cursor := ""
	for {
		raw, err := a.call("members.list", map[string]any{"board": board, "cursor": cursor})
		if err != nil {
			return nil, err
		}
		var page struct {
			Members []boardMember `json:"members"`
			Cursor  string        `json:"next_cursor"`
		}
		if err = json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		members = append(members, page.Members...)
		if cursor = page.Cursor; cursor == "" {
			return members, nil
		}
	}
}

// boardSnapshotExpiry is the hard deadline for a shared snapshot: 7 days
// by default, at most 30. A snapshot on a board always expires.
func boardSnapshotExpiry(v string, now time.Time) (time.Time, error) {
	if v == "" {
		return now.Add(proto.DefaultBoardSnapshotTTL).UTC().Truncate(time.Second), nil
	}
	var t time.Time
	if parsed, err := time.Parse(time.RFC3339, v); err == nil {
		t = parsed
	} else if d, err := parseRetention(v); err == nil {
		t = now.Add(d)
	} else {
		return t, fmt.Errorf("expiry %q: use a duration such as 72h or 7d, or an RFC 3339 time", v)
	}
	t = t.UTC().Truncate(time.Second)
	if !t.After(now) {
		return t, errors.New("expiry is in the past")
	}
	if t.After(now.Add(proto.MaxBoardSnapshotTTL)) {
		return t, fmt.Errorf("a board snapshot expires within %d days", int(proto.MaxBoardSnapshotTTL/(24*time.Hour)))
	}
	return t, nil
}

// snapshotSharePreview is what the human confirms before publishing.
type snapshotSharePreview struct {
	Board      string                  `json:"board"`
	Name       string                  `json:"name"`
	Recipients []string                `json:"recipients"`
	ExpiresAt  time.Time               `json:"expires_at"`
	Summary    *privacy.ConsentSummary `json:"summary"`
	Published  bool                    `json:"published"`
	Item       string                  `json:"item,omitempty"`
	Version    string                  `json:"version,omitempty"`
	Bytes      int64                   `json:"bytes,omitempty"`
}

// snapshotShare previews or publishes a snapshot on a board.
//
//	POST …/{board}/snapshots?snapshot=<library name>|<PAF body>&to=<member>…[&expires=7d][&name=…][&content_digest=<hex>]
//
// Without content_digest it only reports what would be shared: size,
// turns, time range, findings and redactions, the recipients and the
// expiry. With the digest the human reviewed, it records the consent,
// seals the PAF to the recipients and publishes it.
func (a *boardItemAccess) snapshotShare(w http.ResponseWriter, r *http.Request, board string) error {
	q := r.URL.Query()
	src, size, name, err := boardSnapshotSource(w, r, q.Get("snapshot"))
	if err != nil {
		return err
	}
	defer func() { _ = src.Close(); _ = os.Remove(src.Name()) }()
	limits := paf.DefaultLimits()
	arch, err := paf.Open(src, size, paf.Options{Limits: limits, Purpose: paf.PurposeInspect})
	if err != nil {
		return &boardHTTPError{400, "archive", err.Error()}
	}
	if len(arch.Manifest.Agents) != 1 {
		return &boardHTTPError{400, "archive", "a board snapshot holds exactly one agent"}
	}
	findings, err := privacy.ScanArchive(arch, pafContextRecordLimit())
	if err != nil {
		return &boardHTTPError{400, "archive", err.Error()}
	}
	if len(findings) > 0 {
		return &boardHTTPError{422, "flagged_credentials", "suspected credentials must be removed before a snapshot goes on a board: export it, run tclaude paf redact, and share the redacted file"}
	}
	summary, err := privacy.SummarizeForConsent(arch, pafContextRecordLimit())
	if err != nil {
		return &boardHTTPError{400, "archive", err.Error()}
	}
	if n := q.Get("name"); n != "" {
		name = n
	} else if name == "" {
		if ag, ok := arch.Agent(arch.Manifest.Agents[0]); ok {
			name = ag.Name
		}
	}
	if name == "" || len(name) > 128 {
		return &boardHTTPError{400, "name", "name must be 1..128 bytes"}
	}
	now := time.Now()
	expires, err := boardSnapshotExpiry(q.Get("expires"), now)
	if err != nil {
		return &boardHTTPError{400, "expires", err.Error()}
	}
	if err = a.snapshotCapable(); err != nil {
		return err
	}
	members, err := a.boardMembers(board)
	if err != nil {
		return err
	}
	var peers []db.FederationPeer
	if peers, err = db.ListFederationPeers(); err != nil {
		return fmt.Errorf("read trusted peers: %w", err)
	}
	instances, recipients, err := boardSnapshotRecipients(a.opts.Identity, members, q["to"], peers)
	if err != nil {
		return &boardHTTPError{400, "recipients", err.Error()}
	}
	preview := snapshotSharePreview{Board: board, Name: name, Recipients: instances, ExpiresAt: expires, Summary: summary}
	want := q.Get("content_digest")
	if want == "" {
		writeJSON(w, 200, preview)
		return nil
	}
	if want != summary.Digest {
		return &boardHTTPError{409, "consent", "the snapshot changed after it was reviewed (content digest " + summary.Digest + ", reviewed " + want + ")"}
	}

	// Record the human's consent for exactly these recipients, then seal.
	id := a.opts.Identity
	consented, err := os.CreateTemp("", ".board-snapshot-consent-")
	if err != nil {
		return err
	}
	defer func() { _ = consented.Close(); _ = os.Remove(consented.Name()) }()
	keys := []paf.SigningKey{{Key: id.Priv, KeyID: id.ID(), Signer: &paf.Signer{Kind: "instance"}}}
	req := privacy.ConsentRequest{By: pafConsentBy, Audience: privacy.AudienceRecipients, Recipients: instances, KeepExisting: paf.SignedBy(arch.Signatures, id.Pub)}
	if _, err = privacy.AddConsent(arch, consented, req, limits, paf.Producer{Name: "tclaude", Version: buildversion.AppVersion()}, keys...); err != nil {
		return &boardHTTPError{400, "consent", err.Error()}
	}
	if _, err = consented.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var sealed bytes.Buffer
	if err = seal.Encrypt(&sealed, consented, recipients...); err != nil {
		return err
	}
	chunks, err := splitBoardSnapshot(sealed.Bytes())
	if err != nil {
		return &boardHTTPError{413, "too_large", err.Error()}
	}
	state, err := a.call("boards.get", map[string]any{"board": board})
	if err != nil {
		return err
	}
	var b struct {
		Epoch int64 `json:"epoch"`
	}
	if err = json.Unmarshal(state, &b); err != nil {
		return err
	}
	key, err := a.key(board, b.Epoch)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(sealed.Bytes())
	v := proto.BoardSnapshotVersion{Board: board, Item: proto.NewEnvelopeID(), Version: proto.NewEnvelopeID(), Epoch: b.Epoch, PayloadSHA256: hex.EncodeToString(sum[:]), PayloadBytes: int64(sealed.Len()), Recipients: instances, ExpiresAt: expires}
	m := proto.BoardSnapshotManifest{Item: v.Item, Version: v.Version, Name: name, Kind: boardSnapshotKind, PayloadSHA256: v.PayloadSHA256, PayloadBytes: v.PayloadBytes, Recipients: instances, ExpiresAt: expires}
	m.Sign(id)
	metadata, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if v.Metadata, err = proto.SealBoardContent(key, board, b.Epoch, v.Version, metadata); err != nil {
		return err
	}
	ciphers := make([][]byte, len(chunks))
	for i, c := range chunks {
		blob := proto.NewEnvelopeID()
		if ciphers[i], err = proto.SealBoardContent(key, board, b.Epoch, blob, c); err != nil {
			return err
		}
		cs := sha256.Sum256(ciphers[i])
		v.Chunks = append(v.Chunks, proto.BoardSnapshotChunk{Blob: blob, SHA256: hex.EncodeToString(cs[:]), Bytes: int64(len(ciphers[i]))})
	}
	v.Sign(id)
	for i := range ciphers {
		if _, err = client.BoardSnapshotBlob(a.ctx, a.opts, v, i, ciphers[i]); err != nil {
			return fmt.Errorf("upload chunk %d of %d: %w", i+1, len(ciphers), err)
		}
	}
	if _, err = a.call("snapshots.publish", map[string]any{"board": board, "snapshot": v}); err != nil {
		return err
	}
	recordFederationAudit("boards.snapshot.publish", "", "", board, fmt.Sprintf("item=%s version=%s recipients=%s expires=%s", v.Item, v.Version, strings.Join(instances, ","), expires.Format(time.RFC3339)), 200)
	preview.Published, preview.Item, preview.Version, preview.Bytes = true, v.Item, v.Version, v.PayloadBytes
	writeJSON(w, 200, preview)
	return nil
}

// boardSnapshotSource copies the snapshot to share into a private temp
// file: a library snapshot by name, or the request body (a PAF file, for
// example a redacted export).
func boardSnapshotSource(w http.ResponseWriter, r *http.Request, snapshot string) (*os.File, int64, string, error) {
	var in io.Reader
	var name string
	if snapshot != "" {
		meta, path, err := snapshotStore().Get(snapshot)
		if err != nil {
			return nil, 0, "", &boardHTTPError{404, "not_found", "no snapshot " + snapshot}
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, "", err
		}
		defer func() { _ = f.Close() }()
		in, name = f, meta.Name
	} else {
		in = http.MaxBytesReader(w, r.Body, proto.MaxBoardSnapshotBytes+1)
	}
	tmp, err := os.CreateTemp("", ".board-snapshot-src-")
	if err != nil {
		return nil, 0, "", err
	}
	n, err := io.Copy(tmp, io.LimitReader(in, proto.MaxBoardSnapshotBytes+1))
	if err == nil && n > proto.MaxBoardSnapshotBytes {
		err = &boardHTTPError{413, "too_large", fmt.Sprintf("a board snapshot holds at most %d bytes", proto.MaxBoardSnapshotBytes)}
	}
	if err == nil && n == 0 {
		err = &boardHTTPError{400, "archive", "name a library snapshot with snapshot=, or send a PAF"}
	}
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, 0, "", err
	}
	return tmp, n, name, nil
}

// snapshotVersion reads and verifies a snapshot receipt and its publisher
// statement, which must agree.
func (a *boardItemAccess) snapshotVersion(ref boardItemRef) (proto.BoardSnapshotVersion, proto.BoardSnapshotManifest, error) {
	var v proto.BoardSnapshotVersion
	raw, err := a.call("snapshots.get", map[string]any{"board": ref.Board, "item": ref.Item, "version_id": ref.Version})
	if err != nil {
		return v, proto.BoardSnapshotManifest{}, err
	}
	if err = json.Unmarshal(raw, &v); err != nil {
		return v, proto.BoardSnapshotManifest{}, err
	}
	if v.Board != ref.Board || v.Item != ref.Item || v.Version != ref.Version {
		return v, proto.BoardSnapshotManifest{}, errors.New("board snapshot identity mismatch")
	}
	m, err := a.snapshotManifest(v)
	return v, m, err
}

func (a *boardItemAccess) snapshotManifest(v proto.BoardSnapshotVersion) (proto.BoardSnapshotManifest, error) {
	var m proto.BoardSnapshotManifest
	if err := v.Verify(); err != nil {
		return m, err
	}
	key, err := a.key(v.Board, v.Epoch)
	if err != nil {
		return m, err
	}
	raw, err := proto.OpenBoardContent(key, v.Board, v.Epoch, v.Version, v.Metadata)
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if err = m.Verify(); err != nil {
		return m, err
	}
	if m.Item != v.Item || m.Version != v.Version || m.Publisher != v.Publisher || m.PayloadSHA256 != v.PayloadSHA256 || m.PayloadBytes != v.PayloadBytes || !slices.Equal(m.Recipients, v.Recipients) || !m.ExpiresAt.Equal(v.ExpiresAt) {
		return m, errors.New("board snapshot receipt and publisher statement disagree")
	}
	return m, nil
}

// snapshotList lists one page of a board's snapshots.
func (a *boardItemAccess) snapshotList(w http.ResponseWriter, board, cursor string) error {
	raw, err := a.call("items.list", map[string]any{"board": board, "cursor": cursor, "include_snapshots": true})
	if err != nil {
		return err
	}
	var page struct {
		Snapshots []proto.BoardSnapshotVersion `json:"snapshots"`
		Cursor    string                       `json:"next_cursor"`
	}
	if err = json.Unmarshal(raw, &page); err != nil {
		return err
	}
	self := a.opts.Identity.ID()
	rows := []map[string]any{}
	for _, v := range page.Snapshots {
		m, err := a.snapshotManifest(v)
		if err != nil {
			rows = append(rows, map[string]any{"item": v.Item, "version": v.Version, "invalid": true, "error": "snapshot metadata could not be verified"})
			continue
		}
		_, statErr := os.Stat(boardSnapshotSpoolPath(boardItemRef{v.Board, v.Item, v.Version}))
		rows = append(rows, map[string]any{"item": v.Item, "version": v.Version, "name": m.Name, "kind": m.Kind, "publisher": m.Publisher, "recipients": m.Recipients, "for_me": slices.Contains(m.Recipients, self), "expires_at": m.ExpiresAt, "bytes": m.PayloadBytes, "fetched": statErr == nil})
	}
	writeJSON(w, 200, map[string]any{"snapshots": rows, "next_cursor": page.Cursor})
	return nil
}

func boardSnapshotSpoolDir() string {
	return filepath.Join(config.DataDir(), "federation", "board-snapshots")
}

// boardSnapshotSpoolPath is where a fetched, decrypted snapshot waits for
// import, keyed by board, item and version (a publisher picks its version
// ids, so a version id alone is not unique). The ids are validated hex,
// so the name cannot escape the spool.
func boardSnapshotSpoolPath(ref boardItemRef) string {
	return filepath.Join(boardSnapshotSpoolDir(), ref.Board+"-"+ref.Item+"-"+ref.Version+".paf")
}

// boardSnapshotSpoolRecord binds a spooled snapshot to the receipt it was
// fetched under, and carries its expiry so the local copy is removed then.
type boardSnapshotSpoolRecord struct {
	PayloadSHA256 string    `json:"payload_sha256"`
	Publisher     string    `json:"publisher"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func boardSnapshotRecordPath(ref boardItemRef) string {
	return strings.TrimSuffix(boardSnapshotSpoolPath(ref), ".paf") + ".json"
}

func removeBoardSnapshotSpool(ref boardItemRef) {
	_ = os.Remove(boardSnapshotSpoolPath(ref))
	_ = os.Remove(boardSnapshotRecordPath(ref))
}

// writeSpoolFile writes a private file beside its destination, syncs it
// and renames it into place.
func writeSpoolFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".record-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// sweepBoardSnapshotSpool removes local copies whose snapshot has expired,
// copies without a record, and leftovers of interrupted fetches.
func sweepBoardSnapshotSpool(now time.Time) {
	dir := boardSnapshotSpoolDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		switch {
		case strings.HasPrefix(e.Name(), "."):
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > time.Hour {
				_ = os.Remove(path)
			}
		case strings.HasSuffix(e.Name(), ".paf"):
			record := strings.TrimSuffix(path, ".paf") + ".json"
			raw, err := os.ReadFile(record)
			var rec boardSnapshotSpoolRecord
			if err != nil || json.Unmarshal(raw, &rec) != nil || !now.Before(rec.ExpiresAt) {
				_ = os.Remove(path)
				_ = os.Remove(record)
			}
		case strings.HasSuffix(e.Name(), ".json"):
			if _, err := os.Stat(strings.TrimSuffix(path, ".json") + ".paf"); os.IsNotExist(err) {
				_ = os.Remove(path)
			}
		}
	}
}

// snapshotFetch downloads, verifies and decrypts a snapshot addressed to
// this node, then validates and scans the PAF. It never imports.
func (a *boardItemAccess) snapshotFetch(w http.ResponseWriter, ref boardItemRef) error {
	v, m, err := a.snapshotVersion(ref)
	if err != nil {
		return err
	}
	self := a.opts.Identity
	if !slices.Contains(v.Recipients, self.ID()) {
		return &boardHTTPError{403, "not_recipient", "this snapshot is addressed to other board members; this node cannot open it"}
	}
	if !time.Now().Before(v.ExpiresAt) {
		return &boardHTTPError{410, "expired", "this snapshot has expired"}
	}
	key, err := a.key(v.Board, v.Epoch)
	if err != nil {
		return err
	}
	dir := boardSnapshotSpoolDir()
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sealedFile, err := os.CreateTemp(dir, ".sealed-")
	if err != nil {
		return err
	}
	defer func() { _ = sealedFile.Close(); _ = os.Remove(sealedFile.Name()) }()
	want := make([]boardSnapshotChunk, len(v.Chunks))
	for i, c := range v.Chunks {
		want[i] = boardSnapshotChunk{SHA256: c.SHA256, Bytes: c.Bytes}
	}
	err = assembleBoardSnapshot(sealedFile, want, v.PayloadSHA256, v.PayloadBytes,
		func(i int) ([]byte, error) { return client.BoardSnapshotBlob(a.ctx, a.opts, v, i, nil) },
		func(i int, c []byte) ([]byte, error) {
			return proto.OpenBoardContent(key, v.Board, v.Epoch, v.Chunks[i].Blob, c)
		})
	if err != nil {
		return err
	}
	if _, err = sealedFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	ageID, err := seal.Identity(self.Priv)
	if err != nil {
		return err
	}
	plain, err := seal.Decrypt(sealedFile, ageID)
	if err != nil {
		return &boardHTTPError{403, "not_recipient", "this node cannot decrypt the snapshot: " + err.Error()}
	}
	out, err := os.CreateTemp(dir, ".plain-")
	if err != nil {
		return err
	}
	defer func() { _ = out.Close(); _ = os.Remove(out.Name()) }()
	limits := paf.DefaultLimits()
	n, err := io.Copy(out, io.LimitReader(plain, limits.MaxArchiveBytes+1))
	if err != nil {
		return &boardHTTPError{422, "archive", "decrypt: " + err.Error()}
	}
	if n > limits.MaxArchiveBytes {
		return &boardHTTPError{413, "too_large", "the decrypted snapshot exceeds the PAF size limit"}
	}
	summary, err := scanReceivedSnapshot(out, n)
	if err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Chmod(0o600); err != nil {
		return err
	}
	record, err := json.Marshal(boardSnapshotSpoolRecord{PayloadSHA256: v.PayloadSHA256, Publisher: v.Publisher, ExpiresAt: v.ExpiresAt})
	if err != nil {
		return err
	}
	if err = writeSpoolFile(boardSnapshotRecordPath(ref), record); err != nil {
		return err
	}
	if err = os.Rename(out.Name(), boardSnapshotSpoolPath(ref)); err != nil {
		return err
	}
	recordFederationAudit("boards.snapshot.fetch", m.Publisher, "", ref.Board, "item="+ref.Item+" version="+ref.Version, 200)
	writeJSON(w, 200, map[string]any{"state": "ready", "name": m.Name, "publisher": m.Publisher, "expires_at": m.ExpiresAt, "summary": summary})
	return nil
}

// scanReceivedSnapshot validates a received PAF and scans it again: the
// publisher's scan is not trusted.
func scanReceivedSnapshot(f *os.File, size int64) (*privacy.ConsentSummary, error) {
	arch, err := paf.Open(f, size, paf.Options{Limits: paf.DefaultLimits()})
	if err != nil {
		return nil, &boardHTTPError{422, "archive", "the snapshot is not a PAF this node can import: " + err.Error()}
	}
	if len(arch.Manifest.Agents) != 1 {
		return nil, &boardHTTPError{422, "archive", "a board snapshot holds exactly one agent"}
	}
	findings, err := privacy.ScanArchive(arch, pafContextRecordLimit())
	if err != nil {
		return nil, &boardHTTPError{422, "archive", err.Error()}
	}
	if len(findings) > 0 {
		return nil, &boardHTTPError{422, "flagged_credentials", "the received snapshot contains suspected credentials; it was not kept"}
	}
	summary, err := privacy.SummarizeForConsent(arch, pafContextRecordLimit())
	if err != nil {
		return nil, &boardHTTPError{422, "archive", err.Error()}
	}
	return summary, nil
}

type boardSnapshotImportInput struct {
	Group        string   `json:"group"`
	Name         string   `json:"name,omitempty"`
	Cwd          string   `json:"cwd,omitempty"`
	KeepPaths    bool     `json:"keep_paths,omitempty"`
	Harness      string   `json:"harness,omitempty"`
	Model        string   `json:"model,omitempty"`
	Set          []string `json:"set,omitempty"`
	SkipHistory  bool     `json:"skip_history,omitempty"`
	PreviewToken string   `json:"preview_token,omitempty"`
}

// snapshotImport previews or imports a fetched snapshot as a new agent.
// Import needs the single-use token of a preview with the same snapshot
// and options; the PAF is rescanned right before it lands.
func (a *boardItemAccess) snapshotImport(w http.ResponseWriter, r *http.Request, op string, ref boardItemRef) error {
	var in boardSnapshotImportInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		return &boardHTTPError{400, "json", err.Error()}
	}
	if in.Group == "" {
		return &boardHTTPError{400, "group", "group is required: the receiving group"}
	}
	v, m, err := a.snapshotVersion(ref)
	var be *boardHTTPError
	if errors.As(err, &be) && be.Status == http.StatusNotFound {
		removeBoardSnapshotSpool(ref)
		return &boardHTTPError{410, "gone", "this snapshot is no longer on the board (expired or deleted); the local copy was removed"}
	}
	if err != nil {
		return err
	}
	path := boardSnapshotSpoolPath(ref)
	var spooled boardSnapshotSpoolRecord
	raw, err := os.ReadFile(boardSnapshotRecordPath(ref))
	if err != nil || json.Unmarshal(raw, &spooled) != nil {
		return &boardHTTPError{409, "not_fetched", "fetch the snapshot first"}
	}
	if spooled.PayloadSHA256 != v.PayloadSHA256 || spooled.Publisher != v.Publisher || !spooled.ExpiresAt.Equal(v.ExpiresAt) {
		removeBoardSnapshotSpool(ref)
		return &boardHTTPError{409, "not_fetched", "the local copy does not match this snapshot; fetch it again"}
	}
	f, err := os.Open(path)
	if err != nil {
		return &boardHTTPError{409, "not_fetched", "fetch the snapshot first"}
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !time.Now().Before(v.ExpiresAt) {
		removeBoardSnapshotSpool(ref)
		return &boardHTTPError{410, "expired", "this snapshot has expired"}
	}
	summary, err := scanReceivedSnapshot(f, st.Size())
	if err != nil {
		return err
	}
	token := in.PreviewToken
	in.PreviewToken = ""
	options := boardPreviewHash(struct {
		Ref     boardItemRef
		Payload string
		Content string
		Input   boardSnapshotImportInput
	}{ref, v.PayloadSHA256, summary.Digest, in})
	if op == "import" {
		boardPreviewPermits.Lock()
		permit, ok := boardPreviewPermits.permits[token]
		delete(boardPreviewPermits.permits, token)
		boardPreviewPermits.Unlock()
		if !ok || !permit.Expires.After(time.Now()) || permit.Root != config.DataDir() || permit.Options != options {
			return &boardHTTPError{409, "preview_required", "matching unexpired preview token required"}
		}
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	b, err := decodeAgentArchive(f)
	if err != nil {
		return &boardHTTPError{422, "archive", err.Error()}
	}
	source := b.Manifest.Agent
	_ = b.Close()
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	q := url.Values{"group": {in.Group}}
	if op == "import" {
		q.Set("apply", "true")
	}
	for k, val := range map[string]string{"name": in.Name, "cwd": in.Cwd, "harness": in.Harness, "model": in.Model} {
		if val != "" {
			q.Set(k, val)
		}
	}
	if in.KeepPaths {
		q.Set("keep_paths", "true")
	}
	if in.SkipHistory {
		q.Set("skip_history", "true")
	}
	q["set"] = in.Set
	arrival := &arrivalContext{Operation: "board snapshot import", Source: source, Reason: "--cwd or recorded path", UntrustedHistory: true,
		Note: fmt.Sprintf("Shared on a board as %q by %s. You are a new agent forked from that snapshot.", m.Name, m.Publisher)}
	inner := r.Clone(context.WithValue(r.Context(), arrivalContextKey{}, arrival))
	inner.Method = http.MethodPost
	inner.URL = &url.URL{Path: "/v1/agent-bundle/import", RawQuery: q.Encode()}
	inner.Body = f
	inner.ContentLength = st.Size()
	rec := httptest.NewRecorder()
	handleAgentBundleImport(rec, inner)
	if rec.Code != 200 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return nil
	}
	var response map[string]any
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		return err
	}
	response["snapshot"] = map[string]any{"name": m.Name, "publisher": m.Publisher, "expires_at": m.ExpiresAt, "summary": summary}
	if op == "preview" {
		nonce := make([]byte, 24)
		if _, err = rand.Read(nonce); err != nil {
			return err
		}
		token := hex.EncodeToString(nonce)
		boardPreviewPermits.Lock()
		for t, p := range boardPreviewPermits.permits {
			if !p.Expires.After(time.Now()) {
				delete(boardPreviewPermits.permits, t)
			}
		}
		if len(boardPreviewPermits.permits) >= 128 {
			boardPreviewPermits.Unlock()
			return &boardHTTPError{429, "preview_limit", "too many active previews"}
		}
		boardPreviewPermits.permits[token] = boardPreviewPermit{options, "", time.Now().Add(10 * time.Minute), config.DataDir()}
		boardPreviewPermits.Unlock()
		response["preview_token"] = token
	} else {
		removeBoardSnapshotSpool(ref)
		recordFederationAudit("boards.snapshot.import", m.Publisher, "", ref.Board, "item="+ref.Item+" version="+ref.Version, 200)
	}
	writeJSON(w, 200, response)
	return nil
}
