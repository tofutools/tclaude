package agentd

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
)

type bundleContentEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Kind string `json:"kind"`
	open func() (io.ReadCloser, error)
}

// Open the verified inode under the offer lock, then release it before serving.
// Decline may remove its name later; no peer-controlled name becomes a host path.
func openInspectableBundle(w http.ResponseWriter, r *http.Request) (*db.FederationBundleOffer, *os.File) {
	if !requireHuman(w, r, "inspect or download a bundle offer") {
		return nil, nil
	}
	o := lookupReceivingBundleOffer(w, r)
	if o == nil {
		return nil, nil
	}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	current, err := db.GetFederationBundleOffer("in", o.Peer, o.Descriptor.ID)
	if err != nil {
		writeFedErr(w, err)
		return nil, nil
	}
	if current == nil {
		writeError(w, 404, "offer", "no such receiving offer")
		return nil, nil
	}
	if !current.Descriptor.ExpiresAt.After(time.Now()) || current.State == "expired" || current.State == "declined" || current.State == "applied" {
		writeError(w, 410, "offer_state", "offer is no longer available")
		return nil, nil
	}
	if current.State != "ready" {
		writeError(w, 409, "not_fetched", "fetch and verify the offer first")
		return nil, nil
	}
	kind, ok := federationBundleKind(current.Descriptor.Type)
	if !ok || !fedBundleOfferAdmitted(current, kind.Type) {
		writeError(w, 403, "admission", "peer trust or bundle receive grant revoked")
		return nil, nil
	}
	f, err := fedBundleSpool().Open("in", current.Peer, current.Descriptor)
	if err != nil {
		writeError(w, 409, "spool", "verified payload is unavailable; fetch again")
		return nil, nil
	}
	// Reverify to detect preexisting modifications. Published spool files are
	// replaced by rename, so this descriptor pins the checked inode.
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(f, current.Descriptor.Bytes+1))
	if err == nil && (n != current.Descriptor.Bytes || hex.EncodeToString(digest.Sum(nil)) != current.Descriptor.SHA256) {
		err = errors.New("bundle length or SHA-256 mismatch")
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		writeError(w, 409, "spool", err.Error())
		return nil, nil
	}
	return current, f
}

func bundleContentsHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
}

func bundleEntries(f *os.File, d bundletransfer.Descriptor) ([]bundleContentEntry, error) {
	entries := []bundleContentEntry{}
	if d.Type == bundletransfer.Agent.Name {
		z, err := zip.NewReader(f, d.Bytes)
		if err != nil {
			return nil, err
		}
		if len(z.File) > 2 {
			return nil, errors.New("unexpected archive entries")
		}
		seen := map[string]bool{}
		for _, entry := range z.File {
			kind, limit := "json", uint64(agentbundle.MaxManifestBytes)
			switch entry.Name {
			case agentbundle.ManifestFile:
			case agentbundle.HistoryFile:
				kind, limit = "jsonl", uint64(agentbundle.MaxBytes)
			default:
				return nil, errors.New("unexpected archive entry")
			}
			if seen[entry.Name] || !entry.Mode().IsRegular() || entry.UncompressedSize64 > limit {
				return nil, errors.New("invalid archive entry")
			}
			seen[entry.Name] = true
			entries = append(entries, bundleContentEntry{Path: entry.Name, Size: int64(entry.UncompressedSize64), Kind: kind, open: entry.Open})
		}
		if !seen[agentbundle.ManifestFile] {
			return nil, errors.New("archive has no manifest")
		}
	} else {
		raw, err := io.ReadAll(io.LimitReader(f, bundletransfer.Config.MaxBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) > bundletransfer.Config.MaxBytes {
			return nil, errors.New("config bundle too large")
		}
		var b configbundle.Bundle
		if err = json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		if err = b.Validate(); err != nil {
			return nil, err
		}
		add := func(path string, data []byte) {
			entries = append(entries, bundleContentEntry{Path: path, Size: int64(len(data)), Kind: "json", open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }})
		}
		add("bundle.json", raw)
		for section, items := range b.Sections {
			data, err := json.MarshalIndent(items, "", "  ")
			if err != nil {
				return nil, err
			}
			add("sections/"+section+".json", data)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func handleFederationBundleContents(w http.ResponseWriter, r *http.Request) {
	bundleContentsHeaders(w)
	o, f := openInspectableBundle(w, r)
	if f == nil {
		return
	}
	defer f.Close()
	entries, err := bundleEntries(f, o.Descriptor)
	if err != nil {
		writeError(w, 400, "bundle", err.Error())
		return
	}
	path, reading := r.URL.Query()["path"]
	if !reading {
		writeJSON(w, 200, map[string]any{"type": o.Descriptor.Type, "entries": entries})
		return
	}
	if len(path) != 1 {
		writeError(w, 400, "path", "select one entry path")
		return
	}
	var entry *bundleContentEntry
	for i := range entries {
		if entries[i].Path == path[0] {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		writeError(w, 404, "path", "no such bundle entry")
		return
	}
	offset, limit := int64(0), int64(256<<10)
	for key, dst := range map[string]*int64{"offset": &offset, "max_bytes": &limit} {
		if v := r.URL.Query().Get(key); v != "" {
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil {
				writeError(w, 400, key, "invalid "+key)
				return
			}
			*dst = n
		}
	}
	if offset < 0 || offset > entry.Size || limit < 1 || limit > 1<<20 {
		writeError(w, 400, "range", "offset must be within entry and max_bytes must be 1..1048576")
		return
	}
	reader, err := entry.open()
	if err != nil {
		writeError(w, 400, "bundle", err.Error())
		return
	}
	defer reader.Close()
	if _, err = io.CopyN(io.Discard, reader, offset); err != nil {
		writeError(w, 400, "bundle", err.Error())
		return
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit))
	if err != nil {
		writeError(w, 400, "bundle", err.Error())
		return
	}
	next := offset + int64(len(data))
	response := map[string]any{"path": entry.Path, "kind": entry.Kind, "size": entry.Size, "text": strings.ToValidUTF8(string(data), "\uFFFD"), "truncated": next < entry.Size, "offset": offset}
	if next < entry.Size {
		response["next_offset"] = next
	}
	writeJSON(w, 200, response)
}

func handleFederationBundleDownload(w http.ResponseWriter, r *http.Request) {
	bundleContentsHeaders(w)
	o, f := openInspectableBundle(w, r)
	if f == nil {
		return
	}
	defer f.Close()
	ext := "json"
	if o.Descriptor.Type == bundletransfer.Agent.Name {
		ext = "zip"
	}
	// The ID has already passed ValidStreamID; neither peer labels nor summaries
	// are used in this attachment filename.
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="tclaude-%s-%s.%s"`, o.Descriptor.Type, o.Descriptor.ID, ext))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(o.Descriptor.Bytes, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}
