package agentd

import (
	"context"
	"encoding/json"
	"errors"
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
// version it shows.

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
	r.Body = http.MaxBytesReader(w, r.Body, drawingBoardExportBodyLimit)
	var req drawingBoardExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid export request: "+err.Error())
		return
	}
	if len(req.Elements) > drawingBoardMaxLiveElements {
		writeError(w, http.StatusBadRequest, "invalid_arg", "too many element ids")
		return
	}
	for _, e := range req.Elements {
		if !elementIDPattern.MatchString(e) {
			writeError(w, http.StatusBadRequest, "invalid_arg", "invalid element id "+strconv.Quote(e))
			return
		}
	}
	if g := req.Region; g != nil {
		for _, v := range []float64{g.X, g.Y, g.Width, g.Height} {
			if math.IsNaN(v) || math.Abs(v) > drawingBoardMaxCoordinate {
				writeError(w, http.StatusBadRequest, "invalid_arg", "region is out of range")
				return
			}
		}
	}
	board, err := db.GetCollabDrawingBoard(id)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	rows, err := db.ListCollabDrawingBoardElements(id, -1)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	bodies := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		bodies = append(bodies, json.RawMessage(row.Body))
	}
	select {
	case drawingBoardExportSlots <- struct{}{}:
		defer func() { <-drawingBoardExportSlots }()
	case <-r.Context().Done():
		return
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
		return
	case errors.As(err, &missing):
		writeError(w, http.StatusNotFound, "unknown_elements", err.Error())
		return
	case errors.Is(err, drawingboard.ErrNothingToExport):
		writeError(w, http.StatusUnprocessableEntity, "nothing_to_export", "nothing to export: the drawing board, or the region, is empty")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	ext := "png"
	if img.ContentType != "image/png" {
		ext = "svg"
		// An SVG is a document: never let a browser run it from this origin.
		w.Header().Set("Content-Security-Policy", "sandbox")
	}
	w.Header().Set("Content-Type", img.ContentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.`+ext+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Drawing-board-Version", strconv.FormatInt(board.Version, 10))
	w.Header().Set("X-Drawing-board-Export-Elements", strconv.Itoa(len(img.Elements)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(img.Data)
}
