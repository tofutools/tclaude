package hub_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func boardSocket(t *testing.T, url string, id *proto.Identity, token string) (*websocket.Conn, proto.Frame) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(url+proto.BoardWSPath, nil)
	require.NoError(t, err)
	t.Cleanup(func() { ws.Close() })
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ch proto.Frame
	require.NoError(t, ws.ReadJSON(&ch))
	require.NoError(t, ws.WriteJSON(&proto.Frame{Type: proto.FrameHello, Proto: proto.ProtocolVersion, InstanceID: id.ID(), PubKey: id.Pub, Sig: proto.SignHello(id, ch.HubID, ch.Nonce), BoardToken: token}))
	var welcome proto.Frame
	require.NoError(t, ws.ReadJSON(&welcome))
	require.Equal(t, proto.FrameWelcome, welcome.Type, welcome.Message)
	require.Empty(t, welcome.Spaces)
	return ws, ch
}
func boardCall(t *testing.T, ws *websocket.Conn, id *proto.Identity, ch proto.Frame, method string, p any) *proto.HubAdminResult {
	t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	now := time.Now()
	req := &proto.BoardRequest{ID: proto.NewEnvelopeID(), HubID: ch.HubID, Nonce: ch.Nonce, Method: method, Payload: raw, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	req.Sign(id)
	require.NoError(t, ws.WriteJSON(&proto.Frame{Type: proto.FrameBoardRequest, BoardRequest: req}))
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var reply proto.Frame
	require.NoError(t, ws.ReadJSON(&reply))
	require.Equal(t, proto.FrameBoardResult, reply.Type)
	require.NotNil(t, reply.BoardResult)
	return reply.BoardResult
}

// Discover every wire frame from the protocol source instead of copying a
// list that silently stops covering new entry points. Admin RPCs likewise come
// from the actual dispatch map, including methods with no capability gate.
func wireInventory(t *testing.T) ([]string, []string) {
	t.Helper()
	files, err := filepath.Glob("../proto/*.go")
	require.NoError(t, err)
	frames := []string{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			v, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range v.Names {
				if !strings.HasPrefix(name.Name, "Frame") || i >= len(v.Values) {
					continue
				}
				lit, ok := v.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				frames = append(frames, value)
			}
			return true
		})
	}
	f, err := parser.ParseFile(token.NewFileSet(), "admin_rpc.go", nil, 0)
	require.NoError(t, err)
	methods := []string{}
	ast.Inspect(f, func(n ast.Node) bool {
		v, ok := n.(*ast.ValueSpec)
		if !ok || len(v.Names) != 1 || v.Names[0].Name != "adminMethodCapability" {
			return true
		}
		lit, ok := v.Values[0].(*ast.CompositeLit)
		require.True(t, ok)
		for _, elt := range lit.Elts {
			kv := elt.(*ast.KeyValueExpr)
			key := kv.Key.(*ast.BasicLit)
			value, err := strconv.Unquote(key.Value)
			require.NoError(t, err)
			methods = append(methods, value)
		}
		return false
	})
	require.NotEmpty(t, frames)
	require.Contains(t, methods, "claim")
	require.Contains(t, methods, "status")
	return frames, methods
}
func TestBoardOnlyConnectionFullInventory(t *testing.T) {
	_, st, url := newHub(t, hub.Config{Open: true})
	owner, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, st.Admit(owner.ID()))
	ws, ch := boardSocket(t, url, owner, "")
	board := proto.NewEnvelopeID()
	result := boardCall(t, ws, owner, ch, "boards.create", map[string]any{"board": board, "name": "shared", "envelopes": map[string]any{owner.ID(): map[string]any{"ct": "sealed"}}})
	require.Equal(t, 200, result.Status, result.Error)
	bearer := proto.NewEnvelopeID()
	result = boardCall(t, ws, owner, ch, "invites.create", map[string]any{"board": board, "token": bearer, "role": "reader", "ttl_seconds": 120, "key_package": "opaque encrypted package"})
	require.Equal(t, 200, result.Status, result.Error)
	reader, err := proto.NewIdentity()
	require.NoError(t, err)
	rw, rc := boardSocket(t, url, reader, bearer)
	result = boardCall(t, rw, reader, rc, "boards.list", map[string]any{})
	require.Equal(t, 200, result.Status)
	require.Contains(t, string(result.Body), board)
	row, err := st.Get(reader.ID())
	require.NoError(t, err)
	require.Nil(t, row, "joining must not create fleet admission or spaces")
	frames, methods := wireInventory(t)
	for _, frame := range append(frames, "future_unknown_entry") {
		if frame == proto.FrameBoardRequest {
			continue
		}
		t.Run("frame/"+frame, func(t *testing.T) {
			require.NoError(t, rw.WriteJSON(&proto.Frame{Type: frame, AdminRequest: adminRequest(reader, rc, proto.Frame{}, "status", map[string]any{})}))
			rw.SetReadDeadline(time.Now().Add(5 * time.Second))
			var refused proto.Frame
			require.NoError(t, rw.ReadJSON(&refused))
			require.Equal(t, proto.FrameError, refused.Type)
			require.Equal(t, proto.CodeBoardOnly, refused.Code)
		})
	}
	for _, method := range append(methods, "spaces.new_future_method") {
		t.Run("rpc/"+method, func(t *testing.T) {
			require.NoError(t, rw.WriteJSON(&proto.Frame{Type: proto.FrameAdminRequest, AdminRequest: adminRequest(reader, rc, proto.Frame{}, method, map[string]any{"board": board})}))
			rw.SetReadDeadline(time.Now().Add(5 * time.Second))
			var denied proto.Frame
			require.NoError(t, rw.ReadJSON(&denied))
			require.Equal(t, proto.CodeBoardOnly, denied.Code)
			if method == "boards.list" {
				return
			} // Same spelling, separate signature/frame domain.
			result := boardCall(t, rw, reader, rc, method, map[string]any{"board": board})
			require.GreaterOrEqual(t, result.Status, 400)
			require.Empty(t, result.Body)
		})
	}
	// An open relay must not turn existing board membership into fleet admission.
	for _, path := range []string{proto.WSPath, proto.StreamPath} {
		t.Run("endpoint/"+path, func(t *testing.T) {
			s, _, err := websocket.DefaultDialer.Dial(url+path, nil)
			require.NoError(t, err)
			defer s.Close()
			s.SetReadDeadline(time.Now().Add(5 * time.Second))
			var challenge proto.Frame
			require.NoError(t, s.ReadJSON(&challenge))
			require.NoError(t, s.WriteJSON(&proto.Frame{Type: proto.FrameHello, Proto: proto.ProtocolVersion, InstanceID: reader.ID(), PubKey: reader.Pub, Sig: proto.SignHello(reader, challenge.HubID, challenge.Nonce), Stream: proto.NewEnvelopeID(), Peer: owner.ID()}))
			var refused proto.Frame
			require.NoError(t, s.ReadJSON(&refused))
			require.Equal(t, proto.FrameError, refused.Type)
		})
	}
}
