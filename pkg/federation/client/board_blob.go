package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

// BoardBlob transfers stored ciphertext over the bundle stream's authenticated
// record/FIN framing. No peer relay admission is requested or granted.
func BoardBlob(ctx context.Context, opts Options, v proto.BoardItemVersion, upload []byte) ([]byte, error) {
	if e := v.Verify(); e != nil {
		return nil, e
	}
	return boardBlob(ctx, opts, v.Board, v.Blob, v.SHA256, v.Bytes, "", time.Time{}, upload)
}

// BoardSnapshotBlob transfers one verified ciphertext chunk using the existing
// authenticated board stream, with immutable snapshot reservation attributes.
func BoardSnapshotBlob(ctx context.Context, opts Options, v proto.BoardSnapshotVersion, chunkIndex int, upload []byte) ([]byte, error) {
	if err := v.Verify(); err != nil {
		return nil, err
	}
	if chunkIndex < 0 || chunkIndex >= len(v.Chunks) {
		return nil, errors.New("invalid snapshot chunk index")
	}
	c := v.Chunks[chunkIndex]
	return boardBlob(ctx, opts, v.Board, c.Blob, c.SHA256, c.Bytes, "agent-snapshot", v.ExpiresAt, upload)
}

func boardBlob(ctx context.Context, opts Options, board, blob, digest string, size int64, kind string, expiresAt time.Time, upload []byte) ([]byte, error) {
	u, e := ValidateURL(opts.URL)
	if e != nil {
		return nil, e
	}
	u.Path = proto.BoardStreamPath
	u.RawQuery = ""
	u.Fragment = ""
	ws, _, e := (&websocket.Dialer{TLSClientConfig: opts.TLS, HandshakeTimeout: 10 * time.Second}).DialContext(ctx, u.String(), nil)
	if e != nil {
		return nil, e
	}
	defer func() { _ = ws.Close() }()
	deadline := time.Now().Add(5 * time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = ws.SetReadDeadline(deadline)
	_ = ws.SetWriteDeadline(deadline)
	ws.SetReadLimit(proto.MaxAdminResult + 16384)
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	var ch proto.Frame
	if e = ws.ReadJSON(&ch); e != nil {
		return nil, e
	}
	if ch.Type != proto.FrameChallenge || opts.Identity == nil {
		return nil, errors.New("invalid board stream challenge")
	}
	hello := proto.Frame{Type: proto.FrameHello, Proto: proto.ProtocolVersion, InstanceID: opts.Identity.ID(), PubKey: opts.Identity.Pub, Sig: proto.SignHello(opts.Identity, ch.HubID, ch.Nonce)}
	if e = ws.WriteJSON(&hello); e != nil {
		return nil, e
	}
	var welcome proto.Frame
	if e = ws.ReadJSON(&welcome); e != nil {
		return nil, e
	}
	if welcome.Type == proto.FrameError {
		return nil, &RefusedError{welcome.Code, welcome.Message}
	}
	if welcome.Type != proto.FrameWelcome {
		return nil, errors.New("invalid board stream welcome")
	}
	kp, e := stream.NewKeyPair()
	if e != nil {
		return nil, e
	}
	method := "blobs.get"
	if upload != nil {
		method = "blobs.put"
		sum := sha256.Sum256(upload)
		if int64(len(upload)) != size || hex.EncodeToString(sum[:]) != digest {
			return nil, errors.New("board ciphertext descriptor mismatch")
		}
	}
	params := map[string]any{"board": board, "blob": blob, "digest": digest, "bytes": size, "key": kp.Pub}
	if kind != "" {
		params["kind"] = kind
		params["expires_at"] = expiresAt
	}
	raw, _ := json.Marshal(params)
	now := time.Now()
	req := proto.BoardRequest{ID: proto.NewEnvelopeID(), HubID: ch.HubID, Nonce: ch.Nonce, Generation: welcome.AdminGeneration, Method: method, Payload: raw, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	req.Sign(opts.Identity)
	if e = ws.WriteJSON(&proto.Frame{Type: proto.FrameBoardRequest, BoardRequest: &req}); e != nil {
		return nil, e
	}
	var reply proto.Frame
	if e = ws.ReadJSON(&reply); e != nil {
		return nil, e
	}
	if reply.Type != proto.FrameBoardResult || reply.BoardResult == nil || reply.BoardResult.ID != req.ID {
		return nil, errors.New("invalid board stream reply")
	}
	if reply.BoardResult.Status != 200 {
		return nil, fmt.Errorf("board stream refused: %s", reply.BoardResult.Error)
	}
	var result struct {
		Key []byte `json:"key"`
	}
	if e = json.Unmarshal(reply.BoardResult.Body, &result); e != nil {
		return nil, e
	}
	keys, e := stream.DeriveKeys(kp, result.Key, req.ID, true)
	if e != nil {
		return nil, e
	}
	ws.SetReadLimit(proto.MaxStreamMessage + 4096)
	conn, e := stream.New(ws, keys)
	if e != nil {
		return nil, e
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)
	if upload != nil {
		if _, e = io.Copy(conn, bytes.NewReader(upload)); e != nil {
			return nil, e
		}
		if e = conn.CloseWrite(); e != nil {
			return nil, e
		}
		ack, e := io.ReadAll(io.LimitReader(conn, 33))
		if e != nil {
			return nil, e
		}
		if string(ack) != "ok" {
			return nil, errors.New("board upload was not committed")
		}
		return nil, nil
	}
	data, e := io.ReadAll(io.LimitReader(conn, size+1))
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != size || hex.EncodeToString(sum[:]) != digest {
		return nil, errors.New("board ciphertext size or digest mismatch")
	}
	return data, nil
}
