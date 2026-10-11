package agentd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func (a *boardItemAccess) publish(r *http.Request, board string, in boardItemInput) (json.RawMessage, error) {
	var raw []byte
	var m proto.BoardItemManifest
	var e error
	if in.Source != nil {
		if len(in.Only) > 0 || len(in.Skip) > 0 || len(in.Values) > 0 {
			return nil, errors.New("republication preserves exact original payload; export edits as a new item")
		}
		v, original, e := a.version(*in.Source)
		if e != nil {
			return nil, e
		}
		if e = a.fetch(v, original); e != nil {
			return nil, e
		}
		raw, e = a.fetched(v, original)
		if e != nil {
			return nil, e
		}
		m = original
	} else {
		if in.Name == "" || len(in.Name) > 128 {
			return nil, errors.New("name must be 1..128 bytes")
		}
		b, e := collectConfigBundle(r)
		if e != nil {
			return nil, e
		}
		delete(b.Sections, "config")
		delete(b.Sections, "default-permissions")
		if e = b.Select(in.Only, in.Skip); e != nil {
			return nil, e
		}
		if e = b.Prepare(); e != nil {
			return nil, e
		}
		if len(b.Flags) > 0 {
			return nil, errors.New("suspected credentials must be excluded before board publication")
		}
		count := 0
		for _, items := range b.Sections {
			count += len(items)
		}
		if count == 0 {
			return nil, errors.New("select at least one portable config item")
		}
		raw, e = json.Marshal(b)
		if e != nil {
			return nil, e
		}
		if _, e = scanBoardConfig(raw); e != nil {
			return nil, e
		}
		sum := sha256.Sum256(raw)
		m = proto.BoardItemManifest{Item: proto.NewEnvelopeID(), Version: proto.NewEnvelopeID(), Kind: "config", Name: in.Name, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(raw))}
		m.Sign(a.opts.Identity)
	}
	state, e := a.call("boards.get", map[string]any{"board": board})
	if e != nil {
		return nil, e
	}
	var b struct {
		Epoch int64 `json:"epoch"`
	}
	if e = json.Unmarshal(state, &b); e != nil {
		return nil, e
	}
	key, e := a.key(board, b.Epoch)
	if e != nil {
		return nil, e
	}
	v := proto.BoardItemVersion{Board: board, Item: in.Item, Version: proto.NewEnvelopeID(), Parent: in.Parent, Epoch: b.Epoch, Blob: proto.NewEnvelopeID()}
	if v.Item == "" {
		v.Item = proto.NewEnvelopeID()
	}
	ciphertext, e := proto.SealBoardContent(key, board, b.Epoch, v.Blob, raw)
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(ciphertext)
	v.SHA256 = hex.EncodeToString(sum[:])
	v.Bytes = int64(len(ciphertext))
	metadata, e := json.Marshal(m)
	if e != nil {
		return nil, e
	}
	v.Metadata, e = proto.SealBoardContent(key, board, b.Epoch, v.Version, metadata)
	if e != nil {
		return nil, e
	}
	v.Sign(a.opts.Identity)
	if _, e = client.BoardBlob(a.ctx, a.opts, v, ciphertext); e != nil {
		return nil, e
	}
	return a.call("items.publish", map[string]any{"board": board, "version": v})
}

type boardPreviewHashKey struct{}
type boardPreviewPermit struct {
	Options string
	Preview string
	Expires time.Time
	Root    string
}

var boardPreviewPermits = struct {
	sync.Mutex
	permits map[string]boardPreviewPermit
}{permits: map[string]boardPreviewPermit{}}

func boardPreviewHash(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func boardConfigImport(w http.ResponseWriter, r *http.Request, op string, ref boardItemRef, m proto.BoardItemManifest, raw []byte, in boardItemInput) {
	b, e := scanBoardConfig(raw)
	if e != nil {
		writeError(w, 400, "unsafe_bundle", e.Error())
		return
	}
	request := configBundleRequest{Bundle: b, Only: in.Only, Skip: in.Skip, Values: in.Values, Replace: in.Replace}
	options := boardPreviewHash(struct {
		Ref      boardItemRef
		Manifest proto.BoardItemManifest
		Request  configBundleRequest
	}{ref, m, request})
	ctx := r.Context()
	if op == "import" {
		boardPreviewPermits.Lock()
		permit, ok := boardPreviewPermits.permits[in.PreviewToken]
		delete(boardPreviewPermits.permits, in.PreviewToken)
		boardPreviewPermits.Unlock()
		if !ok || !permit.Expires.After(time.Now()) || permit.Root != config.DataDir() || permit.Options != options {
			writeError(w, 409, "preview_required", "matching unexpired preview token required")
			return
		}
		request.Apply = true
		ctx = context.WithValue(ctx, boardPreviewHashKey{}, permit.Preview)
	}
	payload, e := json.Marshal(request)
	if e != nil {
		writeError(w, 400, "json", e.Error())
		return
	}
	inner := r.Clone(ctx)
	inner.Body = io.NopCloser(bytes.NewReader(payload))
	inner.ContentLength = int64(len(payload))
	rec := httptest.NewRecorder()
	handleConfigBundleImport(rec, inner)
	if rec.Code != 200 {
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	var preview configBundlePreview
	if e = json.Unmarshal(rec.Body.Bytes(), &preview); e != nil {
		writeError(w, 500, "preview", e.Error())
		return
	}
	var response map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &response)
	response["provenance"] = m
	if op == "preview" {
		nonce := make([]byte, 24)
		if _, e = rand.Read(nonce); e != nil {
			writeError(w, 500, "preview", e.Error())
			return
		}
		token := hex.EncodeToString(nonce)
		boardPreviewPermits.Lock()
		for token, p := range boardPreviewPermits.permits {
			if !p.Expires.After(time.Now()) {
				delete(boardPreviewPermits.permits, token)
			}
		}
		if len(boardPreviewPermits.permits) >= 128 {
			boardPreviewPermits.Unlock()
			writeError(w, 429, "preview_limit", "too many active previews")
			return
		}
		boardPreviewPermits.permits[token] = boardPreviewPermit{options, boardPreviewHash(preview), time.Now().Add(10 * time.Minute), config.DataDir()}
		boardPreviewPermits.Unlock()
		response["preview_token"] = token
	}
	if op == "import" {
		recordFederationAudit("boards.import", m.Publisher, "", ref.Board, "item="+ref.Item+" version="+ref.Version, 200)
	}
	writeJSON(w, 200, response)
}
