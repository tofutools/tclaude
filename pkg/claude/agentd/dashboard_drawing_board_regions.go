package agentd

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Drawing board event images (tcl-vs77ee): an operator drawing board event can carry a PNG of
// the region the operator changed, rendered by the dashboard editor. The
// daemon stores it as a numbered region next to the drawing board's snapshot, keeps
// the newest drawingBoardMaxRegions, and the event mail names the command that
// fetches it (`tclaude agent drawing-board snapshot <id> --region <n>`), so an agent
// can look at exactly what changed rather than only read about it.
//
// Regions live under one directory per drawing board lifetime (the drawing board's creation
// time), so a drawing board deleted and created again under the same id never hands
// an agent holding older mail an unrelated image under a reused number.

const (
	drawingBoardMaxRegionBytes = 2 << 20
	drawingBoardMaxRegions     = 20
)

func drawingBoardRegionsRoot(id string) string {
	return filepath.Join(filepath.Dir(drawingBoardSnapshotPath(id)), id+".regions")
}

func drawingBoardRegionDir(b *db.CollabDrawingBoard) string {
	return filepath.Join(drawingBoardRegionsRoot(b.ID), strconv.FormatInt(b.CreatedAt.UnixNano(), 10))
}

func drawingBoardRegionPath(b *db.CollabDrawingBoard, n int) string {
	return filepath.Join(drawingBoardRegionDir(b), strconv.Itoa(n)+".png")
}

// drawingBoardRegionNumbers lists a drawing board's stored region numbers, oldest first.
func drawingBoardRegionNumbers(b *db.CollabDrawingBoard) ([]int, error) {
	entries, err := os.ReadDir(drawingBoardRegionDir(b))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range entries {
		n, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".png"))
		if err == nil && n > 0 && strings.HasSuffix(e.Name(), ".png") {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// decodeDrawingBoardRegion decodes an event's base64 PNG. Empty input is no image.
func decodeDrawingBoardRegion(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > drawingBoardMaxRegionBytes+3 {
		return nil, fmt.Errorf("image is larger than %d bytes", drawingBoardMaxRegionBytes)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("image must be base64: %v", err)
	}
	if len(data) > drawingBoardMaxRegionBytes {
		return nil, fmt.Errorf("image is larger than %d bytes", drawingBoardMaxRegionBytes)
	}
	if !bytes.HasPrefix(data, pngMagic) {
		return nil, errors.New("image must be a PNG")
	}
	return data, nil
}

// storeDrawingBoardRegion saves a region image under the next number and drops the
// oldest beyond drawingBoardMaxRegions. The drawing board is re-read under the lock that
// drawing board deletion also takes, so a drawing board deleted meanwhile leaves no images.
func storeDrawingBoardRegion(id string, data []byte) (int, error) {
	drawingBoardSnapshotMu.Lock()
	defer drawingBoardSnapshotMu.Unlock()
	b, err := db.GetCollabDrawingBoard(id)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(drawingBoardRegionDir(b), 0o700); err != nil {
		return 0, err
	}
	nums, err := drawingBoardRegionNumbers(b)
	if err != nil {
		return 0, err
	}
	next := 1
	if len(nums) > 0 {
		next = nums[len(nums)-1] + 1
	}
	if err := writeDrawingBoardSnapshotFile(drawingBoardRegionPath(b, next), data); err != nil {
		return 0, err
	}
	nums = append(nums, next)
	for len(nums) > drawingBoardMaxRegions {
		_ = os.Remove(drawingBoardRegionPath(b, nums[0]))
		nums = nums[1:]
	}
	return next, nil
}

func removeDrawingBoardRegions(id string) error {
	drawingBoardSnapshotMu.Lock()
	defer drawingBoardSnapshotMu.Unlock()
	return os.RemoveAll(drawingBoardRegionsRoot(id))
}

// serveDrawingBoardRegion serves GET .../snapshot?region=N.
func serveDrawingBoardRegion(w http.ResponseWriter, id, raw string) {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "region must be a positive number from a drawing board event")
		return
	}
	b, err := db.GetCollabDrawingBoard(id)
	if err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	data, err := os.ReadFile(drawingBoardRegionPath(b, n))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no_region", fmt.Sprintf("region %d is not stored (only the newest %d are kept); use the full snapshot", n, drawingBoardMaxRegions))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read region: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Drawing-board-Region", strconv.Itoa(n))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
