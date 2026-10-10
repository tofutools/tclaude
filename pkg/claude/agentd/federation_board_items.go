package agentd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type boardItemRef struct {
	Board   string `json:"board"`
	Item    string `json:"item"`
	Version string `json:"version"`
}
type boardItemInput struct {
	Version      string            `json:"version"`
	Name         string            `json:"name"`
	Item         string            `json:"item"`
	Parent       string            `json:"parent"`
	Source       *boardItemRef     `json:"source,omitempty"`
	Only         []string          `json:"only"`
	Skip         []string          `json:"skip"`
	Values       map[string]string `json:"values"`
	Replace      bool              `json:"replace"`
	PreviewToken string            `json:"preview_token"`
}
type boardItemAccess struct {
	opts client.Options
	ctx  context.Context
	keys map[string][]byte
}

func newBoardItemAccess(ctx context.Context) (*boardItemAccess, error) {
	id, e := federationIdentity()
	if e != nil {
		return nil, e
	}
	cfg, e := config.Load()
	if e != nil {
		return nil, e
	}
	if cfg.Federation == nil || cfg.Federation.HubURL == "" {
		return nil, errors.New("configure a federation hub URL")
	}
	opts := client.Options{URL: cfg.Federation.HubURL, Identity: id}
	if cfg.Federation.HubCAFile != "" {
		pem, e := os.ReadFile(cfg.Federation.HubCAFile)
		if e != nil {
			return nil, e
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid hub CA file")
		}
		opts.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &boardItemAccess{opts: opts, ctx: ctx, keys: map[string][]byte{}}, nil
}
func (a *boardItemAccess) call(method string, p any) (json.RawMessage, error) {
	r, e := client.BoardCall(a.ctx, a.opts, "", method, p)
	if e != nil {
		return nil, e
	}
	if r.Status >= 400 {
		return nil, &boardHTTPError{r.Status, r.Code, r.Error}
	}
	return r.Body, nil
}
func (a *boardItemAccess) key(board string, epoch int64) ([]byte, error) {
	k := board + ":" + strconv.FormatInt(epoch, 10)
	if key := a.keys[k]; key != nil {
		return key, nil
	}
	raw, e := a.call("keys.get", map[string]any{"board": board, "epoch": epoch})
	if e != nil {
		return nil, e
	}
	var reply struct {
		Keys map[string]*proto.Encrypted `json:"keys"`
	}
	if e = json.Unmarshal(raw, &reply); e != nil {
		return nil, e
	}
	key, e := proto.OpenBoardKey(a.opts.Identity, board, epoch, reply.Keys[strconv.FormatInt(epoch, 10)])
	if e == nil {
		a.keys[k] = key
	}
	return key, e
}
func (a *boardItemAccess) manifest(v proto.BoardItemVersion) (proto.BoardItemManifest, error) {
	var m proto.BoardItemManifest
	if e := v.Verify(); e != nil {
		return m, e
	}
	key, e := a.key(v.Board, v.Epoch)
	if e != nil {
		return m, e
	}
	raw, e := proto.OpenBoardContent(key, v.Board, v.Epoch, v.Version, v.Metadata)
	if e != nil {
		return m, e
	}
	if e = json.Unmarshal(raw, &m); e != nil {
		return m, e
	}
	return m, m.Verify()
}
func (a *boardItemAccess) version(ref boardItemRef) (proto.BoardItemVersion, proto.BoardItemManifest, error) {
	var v proto.BoardItemVersion
	var m proto.BoardItemManifest
	raw, e := a.call("items.get", map[string]any{"board": ref.Board, "item": ref.Item, "version_id": ref.Version})
	if e != nil {
		return v, m, e
	}
	if e = json.Unmarshal(raw, &v); e != nil {
		return v, m, e
	}
	if v.Board != ref.Board || v.Item != ref.Item || v.Version != ref.Version {
		return v, m, errors.New("board version identity mismatch")
	}
	m, e = a.manifest(v)
	return v, m, e
}
func boardItemSpool() bundletransfer.Spool {
	return bundletransfer.Spool{Root: filepath.Join(config.DataDir(), "federation", "board-items")}
}
func boardPlainDescriptor(v proto.BoardItemVersion, m proto.BoardItemManifest) bundletransfer.Descriptor {
	return bundletransfer.Descriptor{ID: v.Version, Type: "config", Bytes: m.Bytes, SHA256: m.SHA256, ExpiresAt: time.Now().Add(time.Hour)}
}
func scanBoardConfig(raw []byte) (configbundle.Bundle, error) {
	var b configbundle.Bundle
	if e := json.Unmarshal(raw, &b); e != nil {
		return b, e
	}
	if e := b.Validate(); e != nil {
		return b, e
	}
	for section, items := range b.Sections {
		if len(items) > 0 && (section == "config" || section == "default-permissions") {
			return b, errors.New("board imports do not include broad config or default permissions")
		}
	}
	before, e := json.Marshal(b)
	if e != nil {
		return b, e
	}
	if e = b.Prepare(); e != nil {
		return b, e
	}
	after, e := json.Marshal(b)
	if e != nil {
		return b, e
	}
	if len(b.Flags) > 0 || !bytes.Equal(before, after) {
		return b, errors.New("board payload contains credentials or nonportable structured fields")
	}
	return b, nil
}
func (a *boardItemAccess) fetched(v proto.BoardItemVersion, m proto.BoardItemManifest) ([]byte, error) {
	raw, e := boardItemSpool().Read("in", v.Publisher, boardPlainDescriptor(v, m))
	if e != nil {
		return nil, &boardHTTPError{409, "not_fetched", "fetch and verify the version first"}
	}
	if e = m.VerifyPayload(raw); e != nil {
		return nil, e
	}
	_, e = scanBoardConfig(raw)
	return raw, e
}
func (a *boardItemAccess) fetch(v proto.BoardItemVersion, m proto.BoardItemManifest) error {
	cipher, e := client.BoardBlob(a.ctx, a.opts, v, nil)
	if e != nil {
		return e
	}
	key, e := a.key(v.Board, v.Epoch)
	if e != nil {
		return e
	}
	raw, e := proto.OpenBoardContent(key, v.Board, v.Epoch, v.Blob, cipher)
	if e != nil {
		return e
	}
	if e = m.VerifyPayload(raw); e != nil {
		return e
	}
	if _, e = scanBoardConfig(raw); e != nil {
		return e
	}
	return boardItemSpool().Receive("in", v.Publisher, boardPlainDescriptor(v, m), bytes.NewReader(raw))
}
func registerBoardItemRoutes(mux *http.ServeMux, prefix string, dashboard bool) {
	for pattern, op := range map[string]string{"GET /{board}/items": "list", "POST /{board}/items": "publish", "GET /{board}/items/{item}/versions": "versions", "PUT /{board}/items/{item}/pin": "pin", "POST /{board}/items/{item}/versions/{version}/fetch": "fetch", "GET /{board}/items/{item}/versions/{version}/contents": "contents", "GET /{board}/items/{item}/versions/{version}/download": "download", "HEAD /{board}/items/{item}/versions/{version}/download": "download", "POST /{board}/items/{item}/versions/{version}/preview": "preview", "POST /{board}/items/{item}/versions/{version}/import": "import"} {
		method, tail, _ := strings.Cut(pattern, " ")
		handler := boardItemRoute(op)
		if dashboard {
			handler = dashboardFederationRoute(handler)
		}
		mux.HandleFunc(method+" "+prefix+tail, handler)
	}
}
func boardItemRoute(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHuman(w, r, "publish, inspect or import board items") {
			return
		}
		bundleContentsHeaders(w)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		a, e := newBoardItemAccess(ctx)
		if e != nil {
			writeError(w, 409, "board_unavailable", e.Error())
			return
		}
		fail := func(e error) {
			var be *boardHTTPError
			if errors.As(e, &be) {
				writeError(w, be.Status, be.Code, be.Message)
			} else {
				writeError(w, 502, "board_item", e.Error())
			}
		}
		ref := boardItemRef{r.PathValue("board"), r.PathValue("item"), r.PathValue("version")}
		in := boardItemInput{}
		if r.Method == "POST" || r.Method == "PUT" {
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
			if e = dec.Decode(&in); e != nil {
				writeError(w, 400, "json", e.Error())
				return
			}
			var extra any
			if dec.Decode(&extra) != io.EOF {
				writeError(w, 400, "json", "expected one object")
				return
			}
		}
		switch op {
		case "list", "versions":
			method := "items.list"
			if op == "versions" {
				method = "items.versions"
			}
			raw, e := a.call(method, map[string]any{"board": ref.Board, "item": ref.Item, "cursor": r.URL.Query().Get("cursor")})
			if e != nil {
				fail(e)
				return
			}
			var result struct {
				Items    []proto.BoardItemVersion `json:"items"`
				Versions []proto.BoardItemVersion `json:"versions"`
				Cursor   string                   `json:"next_cursor"`
			}
			if e = json.Unmarshal(raw, &result); e != nil {
				fail(e)
				return
			}
			entries := result.Items
			key := "items"
			if op == "versions" {
				entries = result.Versions
				key = "versions"
			}
			pins := map[string]string{}
			if op == "list" {
				cursor := ""
				for {
					raw, e := a.call("pins.list", map[string]any{"board": ref.Board, "cursor": cursor})
					if e != nil {
						fail(e)
						return
					}
					var page struct {
						Pins []struct {
							Item    string `json:"item"`
							Version string `json:"version"`
						} `json:"pins"`
						Cursor string `json:"next_cursor"`
					}
					if e = json.Unmarshal(raw, &page); e != nil {
						fail(e)
						return
					}
					for _, pin := range page.Pins {
						pins[pin.Item] = pin.Version
					}
					cursor = page.Cursor
					if cursor == "" {
						break
					}
				}
			}
			rows := []map[string]any{}
			for _, v := range entries {
				m, e := a.manifest(v)
				if e != nil {
					// A publisher's opaque metadata must not hide other items or
					// prevent continuing the hub's bounded catalog pagination.
					rows = append(rows, map[string]any{"id": v.Item, "version": v.Version, "name": "Invalid item metadata", "invalid": true, "error": "publisher metadata could not be verified"})
					continue
				}
				rows = append(rows, map[string]any{"id": v.Item, "version": v.Version, "name": m.Name, "kind": m.Kind, "publisher": m.Publisher, "republisher": v.Publisher, "digest": m.SHA256, "bytes": m.Bytes, "provenance": m, "latest_version": v.Version, "pinned_version": pins[v.Item], "update_available": pins[v.Item] != "" && pins[v.Item] != v.Version})
			}
			writeJSON(w, 200, map[string]any{key: rows, "next_cursor": result.Cursor})
			return
		case "publish":
			body, e := a.publish(r, ref.Board, in)
			if e != nil {
				fail(e)
				return
			}
			recordFederationAudit("boards.publish", "", "", ref.Board, "signed config item", 200)
			var out any
			_ = json.Unmarshal(body, &out)
			writeJSON(w, 200, out)
			return
		case "pin":
			raw, e := a.call("pins.set", map[string]any{"board": ref.Board, "item": ref.Item, "version_id": in.Version})
			if e != nil {
				fail(e)
				return
			}
			var out any
			_ = json.Unmarshal(raw, &out)
			writeJSON(w, 200, out)
			return
		}
		v, m, e := a.version(ref)
		if e != nil {
			fail(e)
			return
		}
		if op == "fetch" {
			if e = a.fetch(v, m); e != nil {
				fail(e)
				return
			}
			recordFederationAudit("boards.fetch", m.Publisher, "", ref.Board, "item="+ref.Item+" version="+ref.Version, 200)
			writeJSON(w, 200, map[string]any{"state": "ready", "version": v.Version, "provenance": m})
			return
		}
		raw, e := a.fetched(v, m)
		if e != nil {
			fail(e)
			return
		}
		switch op {
		case "contents":
			f, e := boardItemSpool().Open("in", v.Publisher, boardPlainDescriptor(v, m))
			if e != nil {
				fail(e)
				return
			}
			defer func() { _ = f.Close() }()
			serveBundleContents(w, r, f, boardPlainDescriptor(v, m))
		case "download":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="board-%s.json"`, v.Version))
			w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
			w.WriteHeader(200)
			if r.Method != "HEAD" {
				_, _ = w.Write(raw)
			}
		case "preview", "import":
			boardConfigImport(w, r, op, ref, m, raw, in)
		}
	}
}
