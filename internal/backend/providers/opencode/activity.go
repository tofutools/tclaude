package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/tofutools/tclaude/internal/backend/ports"
)

// observeActivity reads the same native session status and attention endpoints
// as v1. A healthy server alone says nothing about its session's activity.
func (r *Runtime) observeActivity(ctx context.Context) (ports.AgentActivityObservedState, time.Time) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	unknown := func() (ports.AgentActivityObservedState, time.Time) { return ports.AgentActivityUnknown, time.Time{} }
	var current sessionRecord
	if r.readActivityJSON(ctx, "/session/"+url.PathEscape(r.nativeID), &current) != nil {
		return unknown()
	}
	parent := ""
	if current.ParentID != nil {
		parent = *current.ParentID
	}
	if current.ID != r.nativeID || parent != r.parentID || filepath.Clean(current.Directory) != filepath.Clean(r.cwd) {
		return unknown()
	}
	var statuses map[string]struct {
		Type string `json:"type"`
	}
	if r.readActivityJSON(ctx, "/session/status", &statuses) != nil || statuses == nil {
		return unknown()
	}
	waiting := false
	for _, path := range []string{"/question", "/permission"} {
		var pending []struct {
			SessionID string `json:"sessionID"`
		}
		if r.readActivityJSON(ctx, path, &pending) != nil || pending == nil {
			return unknown()
		}
		for _, item := range pending {
			if item.SessionID == r.nativeID {
				waiting = true
			}
		}
	}
	if waiting {
		return ports.AgentActivityAwaitingInput, time.Now().UTC()
	}
	status, exists := statuses[r.nativeID]
	// OpenCode removes idle sessions from this map. The exact session and both
	// attention lists must still have been read successfully before using that.
	if !exists || status.Type == "idle" {
		return ports.AgentActivityIdle, time.Now().UTC()
	}
	if status.Type == "busy" || status.Type == "retry" {
		return ports.AgentActivityActive, time.Now().UTC()
	}
	return unknown()
}

func (r *Runtime) readActivityJSON(ctx context.Context, path string, value any) error {
	response, err := r.do(ctx, http.MethodGet, path+"?directory="+url.QueryEscape(r.cwd), nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("OpenCode activity returned HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(value)
}
