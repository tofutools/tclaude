package agentd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/jobstream"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const maxJobOutputReply = 2 << 20

type fedJobOutputCursor struct {
	Job         string `json:"job"`
	Fingerprint string `json:"fingerprint"`
	Offset      int64  `json:"offset"`
	jobstream.Cursor
}
type fedJobOutputRequest struct {
	bundletransfer.Request
	Cursor   fedJobOutputCursor `json:"cursor"`
	MaxBytes int                `json:"max_bytes"`
}

func (r fedJobOutputRequest) valid(j *db.FederationJob) bool {
	return r.MaxBytes >= jobstream.MaxChunk && r.MaxBytes <= 256<<10 && r.Cursor.Job == j.ID && r.Cursor.Fingerprint == j.Fingerprint && r.Cursor.Offset >= 0 && r.Cursor.Offset <= jobstream.MaxEncodedBytes && r.Cursor.Stdout <= jobstream.MaxChannel && r.Cursor.Stderr <= jobstream.MaxChannel
}

type fedJobOutputChunk struct {
	Stream   string `json:"stream"`
	Data     string `json:"data"`
	Encoding string `json:"encoding"`
}
type fedJobOutputResponse struct {
	Chunks    []fedJobOutputChunk `json:"chunks"`
	Cursor    string              `json:"cursor"`
	Done      bool                `json:"done"`
	State     string              `json:"state"`
	Truncated bool                `json:"truncated,omitempty"`
}

func encodeJobOutputCursor(c fedJobOutputCursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}
func decodeJobOutputCursor(raw string, j *db.FederationJob) (fedJobOutputCursor, error) {
	c := fedJobOutputCursor{Job: j.ID, Fingerprint: j.Fingerprint}
	if raw == "" {
		return c, nil
	}
	if len(raw) > 1024 {
		return c, errors.New("invalid output cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	if err != nil || !(fedJobOutputRequest{Cursor: c, MaxBytes: jobstream.MaxChunk}).valid(j) {
		return c, errors.New("invalid output cursor")
	}
	return c, nil
}

// Seek directly to a previously validated complete frame boundary. Snapshot
// the file size once: partial concurrent appends are retried on the next poll,
// never waited on, and neither previous output nor a partial frame is replayed.
func readFederationJobOutput(f *os.File, j *db.FederationJob, c fedJobOutputCursor, maxBytes int) (fedJobOutputResponse, error) {
	out := fedJobOutputResponse{Chunks: []fedJobOutputChunk{}, State: j.State}
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	if c.Offset > info.Size() {
		return out, errors.New("cursor past available output")
	}
	reader := io.NewSectionReader(f, c.Offset, info.Size()-c.Offset)
	used := 0
	for len(out.Chunks) < 256 {
		before, err := reader.Seek(0, io.SeekCurrent)
		if err != nil {
			return out, err
		}
		frame, err := jobstream.Read(reader)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return out, err
		}
		if used+len(frame.Data) > maxBytes {
			break
		}
		streamName := "stdout"
		if frame.Channel == jobstream.Stderr {
			streamName = "stderr"
		}
		expected := c.Stdout
		if frame.Channel == jobstream.Stderr {
			expected = c.Stderr
		}
		if frame.Offset != expected {
			return out, jobstream.ErrGap
		}
		if err = c.Cursor.Apply(frame, io.Discard, io.Discard); err != nil {
			return out, err
		}
		out.Chunks = append(out.Chunks, fedJobOutputChunk{Stream: streamName, Data: base64.StdEncoding.EncodeToString(frame.Data), Encoding: "base64"})
		used += len(frame.Data)
		after, _ := reader.Seek(0, io.SeekCurrent)
		c.Offset += after - before
	}
	out.Cursor = encodeJobOutputCursor(c)
	out.Truncated = c.Offset < info.Size()
	return out, nil
}

func handleFederationJobOutput(w http.ResponseWriter, r *http.Request) {
	j, err := db.GetFederationJob(r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "job", "job not found")
		return
	}
	if !authorizeJobAccess(w, r, j) {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	c, err := decodeJobOutputCursor(r.URL.Query().Get("cursor"), j)
	if err != nil {
		writeError(w, 400, "cursor", err.Error())
		return
	}
	limit := 64 << 10
	if raw := r.URL.Query().Get("max_bytes"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < jobstream.MaxChunk || limit > 256<<10 {
			writeError(w, 400, "max_bytes", "max_bytes must be 32768 through 262144")
			return
		}
	}
	if j.Direction != "out" {
		writeError(w, 409, "job", "output polling requires a submitted job")
		return
	}
	if jobTerminal(j.State) {
		writeJSON(w, 200, fedJobOutputResponse{Chunks: []fedJobOutputChunk{}, Cursor: encodeJobOutputCursor(c), Done: true, State: j.State})
		return
	}
	if j.State == "pending" || j.State == "preparing" {
		writeError(w, 409, "job", "job has not started")
		return
	}
	raw, _, err := db.GetFederationCatalog(j.Peer)
	var catalog proto.CatalogPayload
	if err != nil || json.Unmarshal([]byte(raw), &catalog) != nil || !catalog.JobOutput {
		writeError(w, 409, "unsupported_peer", "peer does not support incremental job output")
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 503, "offline", "federation offline")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	conn, err := rt.openJobFollowRequest(ctx, j, &fedJobOutputRequest{Cursor: c, MaxBytes: limit})
	if err != nil {
		writeError(w, 502, "output", err.Error())
		return
	}
	defer conn.Close()
	data, err := io.ReadAll(io.LimitReader(conn, maxJobOutputReply+1))
	if err != nil || len(data) > maxJobOutputReply {
		writeError(w, 502, "output", "invalid or incomplete output reply")
		return
	}
	var out fedJobOutputResponse
	if json.Unmarshal(data, &out) != nil || len(out.Chunks) > 256 {
		writeError(w, 502, "output", "invalid output reply")
		return
	}
	next, err := decodeJobOutputCursor(out.Cursor, j)
	size := 0
	for _, chunk := range out.Chunks {
		if chunk.Stream != "stdout" && chunk.Stream != "stderr" {
			err = errors.New("invalid channel")
		}
		decoded, decodeErr := base64.StdEncoding.DecodeString(chunk.Data)
		if decodeErr != nil || chunk.Encoding != "base64" {
			err = errors.New("invalid output encoding")
		}
		size += len(decoded)
	}
	if err != nil || next.Offset < c.Offset || next.Stdout < c.Stdout || next.Stderr < c.Stderr || size > limit {
		writeError(w, 502, "output", "invalid output cursor or chunks")
		return
	}
	p, err := db.GetFederationPeer(j.Peer)
	if err != nil || p == nil {
		writeError(w, 403, "peer", "peer no longer trusted")
		return
	}
	current, err := db.GetFederationJob(j.ID)
	if err != nil {
		writeError(w, 503, "job", "job status unavailable")
		return
	}
	if !authorizeJobAccess(w, r, current) {
		return
	}
	// Exit is authoritative only after our existing terminal receipt/log checks.
	out.State = current.State
	out.Done = jobTerminal(current.State)
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, out)
}
