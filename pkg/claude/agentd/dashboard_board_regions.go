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

// Board event images (tcl-vs77ee): an operator board event can carry a PNG of
// the region the operator changed, rendered by the dashboard editor. The
// daemon stores it as a numbered region next to the board's snapshot, keeps
// the newest boardMaxRegions, and the event mail names the command that
// fetches it (`tclaude agent board snapshot <id> --region <n>`), so an agent
// can look at exactly what changed rather than only read about it.

const (
	boardMaxRegionBytes = 2 << 20
	boardMaxRegions     = 20
)

func boardRegionDir(id string) string {
	return filepath.Join(filepath.Dir(boardSnapshotPath(id)), id+".regions")
}

func boardRegionPath(id string, n int) string {
	return filepath.Join(boardRegionDir(id), strconv.Itoa(n)+".png")
}

// boardRegionNumbers lists a board's stored region numbers, oldest first.
func boardRegionNumbers(id string) ([]int, error) {
	entries, err := os.ReadDir(boardRegionDir(id))
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

// decodeBoardRegion decodes an event's base64 PNG. Empty input is no image.
func decodeBoardRegion(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > boardMaxRegionBytes+3 {
		return nil, fmt.Errorf("image is larger than %d bytes", boardMaxRegionBytes)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("image must be base64: %v", err)
	}
	if len(data) > boardMaxRegionBytes {
		return nil, fmt.Errorf("image is larger than %d bytes", boardMaxRegionBytes)
	}
	if !bytes.HasPrefix(data, pngMagic) {
		return nil, errors.New("image must be a PNG")
	}
	return data, nil
}

// storeBoardRegion saves a region image under the next number and drops the
// oldest beyond boardMaxRegions.
func storeBoardRegion(id string, data []byte) (int, error) {
	boardSnapshotMu.Lock()
	defer boardSnapshotMu.Unlock()
	if err := os.MkdirAll(boardRegionDir(id), 0o700); err != nil {
		return 0, err
	}
	nums, err := boardRegionNumbers(id)
	if err != nil {
		return 0, err
	}
	next := 1
	if len(nums) > 0 {
		next = nums[len(nums)-1] + 1
	}
	if err := writeBoardSnapshotFile(boardRegionPath(id, next), data); err != nil {
		return 0, err
	}
	nums = append(nums, next)
	for len(nums) > boardMaxRegions {
		_ = os.Remove(boardRegionPath(id, nums[0]))
		nums = nums[1:]
	}
	return next, nil
}

func removeBoardRegions(id string) error {
	boardSnapshotMu.Lock()
	defer boardSnapshotMu.Unlock()
	return os.RemoveAll(boardRegionDir(id))
}

// serveBoardRegion serves GET .../snapshot?region=N.
func serveBoardRegion(w http.ResponseWriter, id, raw string) {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "region must be a positive number from a board event")
		return
	}
	if _, err := db.GetCollabBoard(id); err != nil {
		writeBoardStoreError(w, err)
		return
	}
	data, err := os.ReadFile(boardRegionPath(id, n))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no_region", fmt.Sprintf("region %d is not stored (only the newest %d are kept); use the full snapshot", n, boardMaxRegions))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read region: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Board-Region", strconv.Itoa(n))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
