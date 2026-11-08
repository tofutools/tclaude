package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/drawingboard"
)

// Drawing-board export (tk_413bdf5d): the board, or part of it, rendered by
// the daemon from its stored elements (pkg/claude/drawingboard) as PNG or SVG.
// Unlike the snapshot, which is whatever an open dashboard last uploaded,
// an export is the board as it is now and needs no browser.
//
//	POST /v1/drawing-boards/{id}/export   agents: drawing-board read access
//	POST /api/drawing-boards/{id}/export  the dashboard
//
// Body: {format: "png"|"svg", elements?: [ids], region?: {x,y,width,height},
// scale?: n}. The reply is the image; X-Drawing-board-Version names the board
// version it shows. ?format=json answers it as JSON instead, the image
// base64-encoded, for a peer view (which carries JSON only) and a script.

const drawingBoardExportBodyLimit = 256 << 10

// drawingBoardExportSlots bounds the renders running at once: a PNG at the
// size cap holds a 64 MiB canvas.
var drawingBoardExportSlots = make(chan struct{}, 2)

// drawingBoardExportDeadline bounds one render, so a board that is costly to
// draw cannot hold a slot.
const drawingBoardExportDeadline = 30 * time.Second

type drawingBoardExportRequest struct {
	Format   string            `json:"format"`
	Elements []string          `json:"elements"`
	Region   *drawingboard.Box `json:"region"`
	Scale    float64           `json:"scale"`
}

func handleDrawingBoardExport(w http.ResponseWriter, r *http.Request) {
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	if _, ok := requireDrawingBoardAccess(w, r, id, false); ok {
		serveDrawingBoardExport(w, r, id)
	}
}

func handleDashboardDrawingBoardExport(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if id, ok := drawingBoardPathID(w, r); ok {
		serveDrawingBoardExport(w, r, id)
	}
}

func serveDrawingBoardExport(w http.ResponseWriter, r *http.Request, id string) {
	serveDrawingBoardExportWith(w, r, id, drawingBoardExportServe{asJSON: r.URL.Query().Get("format") == "json"})
}

// drawingBoardExportServe is how an export is answered.
type drawingBoardExportServe struct {
	// asJSON answers drawingBoardExportJSON, the image base64-encoded in
	// it, instead of the image itself: what a peer view carries.
	asJSON bool
	// maxBytes, when set, refuses a bigger image (a peer view's body cap).
	maxBytes int
}

// drawingBoardExportJSON is an export as JSON (?format=json).
type drawingBoardExportJSON struct {
	ContentType string           `json:"content_type"`
	Filename    string           `json:"filename"`
	Data        []byte           `json:"data"` // base64
	Width       int              `json:"width"`
	Height      int              `json:"height"`
	Version     int64            `json:"version"`
	Elements    int              `json:"elements"`
	Crop        drawingboard.Box `json:"crop"`
}

func serveDrawingBoardExportWith(w http.ResponseWriter, r *http.Request, id string, how drawingBoardExportServe) {
	r.Body = http.MaxBytesReader(w, r.Body, drawingBoardExportBodyLimit)
	var req drawingBoardExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid export request: "+err.Error())
		return
	}
	img, version, ok := renderDrawingBoardExport(w, r, id, req)
	if !ok {
		return
	}
	if how.maxBytes > 0 && len(img.Data) > how.maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("the export is larger than %d MiB, more than a linked node carries; export a part of it, or at a smaller scale", how.maxBytes>>20))
		return
	}
	ext := "png"
	if img.ContentType != "image/png" {
		ext = "svg"
	}
	filename := id + "." + ext
	if how.asJSON {
		writeJSON(w, http.StatusOK, drawingBoardExportJSON{ContentType: img.ContentType, Filename: filename, Data: img.Data,
			Width: img.Width, Height: img.Height, Version: version, Elements: len(img.Elements), Crop: img.Crop})
		return
	}
	if ext == "svg" {
		// An SVG is a document: never let a browser run it from this origin.
		w.Header().Set("Content-Security-Policy", "sandbox")
	}
	w.Header().Set("Content-Type", img.ContentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Drawing-board-Version", strconv.FormatInt(version, 10))
	w.Header().Set("X-Drawing-board-Export-Elements", strconv.Itoa(len(img.Elements)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(img.Data)
}

// renderDrawingBoardExport checks an export request and renders it, from
// the board as stored now. It writes the refusal itself; version is the
// board version the image shows.
func renderDrawingBoardExport(w http.ResponseWriter, r *http.Request, id string, req drawingBoardExportRequest) (*drawingboard.Image, int64, bool) {
	if len(req.Elements) > drawingBoardMaxLiveElements {
		writeError(w, http.StatusBadRequest, "invalid_arg", "too many element ids")
		return nil, 0, false
	}
	for _, e := range req.Elements {
		if !elementIDPattern.MatchString(e) {
			writeError(w, http.StatusBadRequest, "invalid_arg", "invalid element id "+strconv.Quote(e))
			return nil, 0, false
		}
	}
	if g := req.Region; g != nil {
		for _, v := range []float64{g.X, g.Y, g.Width, g.Height} {
			if math.IsNaN(v) || math.Abs(v) > drawingBoardMaxCoordinate {
				writeError(w, http.StatusBadRequest, "invalid_arg", "region is out of range")
				return nil, 0, false
			}
		}
	}
	board, err := db.GetCollabDrawingBoard(id)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return nil, 0, false
	}
	rows, err := db.ListCollabDrawingBoardElements(id, -1)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return nil, 0, false
	}
	bodies := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		bodies = append(bodies, json.RawMessage(row.Body))
	}
	select {
	case drawingBoardExportSlots <- struct{}{}:
		defer func() { <-drawingBoardExportSlots }()
	case <-r.Context().Done():
		// Gone, or (a peer's export) out of time waiting for a slot.
		writeError(w, http.StatusServiceUnavailable, "export_timeout",
			"other exports are drawing; try again in a moment, or export a part of the drawing board")
		return nil, 0, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), drawingBoardExportDeadline)
	defer cancel()
	img, err := drawingboard.Render(ctx, drawingboard.DecodeElements(bodies), drawingboard.Options{
		Format: req.Format, Elements: req.Elements, Region: req.Region, Scale: req.Scale,
	})
	var missing *drawingboard.MissingElementsError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		writeError(w, http.StatusServiceUnavailable, "export_timeout",
			"the export took too long to draw; export a part of the drawing board, or at a smaller scale")
		return nil, 0, false
	case errors.As(err, &missing):
		writeError(w, http.StatusNotFound, "unknown_elements", err.Error())
		return nil, 0, false
	case errors.Is(err, drawingboard.ErrNothingToExport):
		writeError(w, http.StatusUnprocessableEntity, "nothing_to_export", "nothing to export: the drawing board, or the region, is empty")
		return nil, 0, false
	case err != nil:
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return nil, 0, false
	}
	return img, board.Version, true
}
