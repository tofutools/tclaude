package nativeactivity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type checkpoint struct {
	SessionID  string
	State      ports.AgentActivityObservedState
	ObservedAt time.Time
}

// Save precedes acknowledgement of a native hook. The private attempt directory
// binds this record to one execution; SessionID additionally binds its context.
func (s *State) Save(directory, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	value, at := s.Observation()
	raw, err := json.Marshal(checkpoint{SessionID: sessionID, State: value, ObservedAt: at})
	if err != nil {
		return err
	}
	if err := host.WriteProtectedFile(filepath.Join(directory, "activity.json"), raw); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s *State) Restore(directory, sessionID string) error {
	raw, err := host.ReadProtectedFile(filepath.Join(directory, "activity.json"), 4096)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved checkpoint
	if err := json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	if saved.SessionID != sessionID || sessionID == "" {
		return nil
	}
	switch saved.State {
	case ports.AgentActivityUnknown:
	case ports.AgentActivityActive, ports.AgentActivityIdle, ports.AgentActivityAwaitingInput:
		if saved.ObservedAt.IsZero() {
			return fmt.Errorf("native activity checkpoint has no observation time")
		}
	default:
		return fmt.Errorf("native activity checkpoint has unknown state")
	}
	s.value, s.at = saved.State, saved.ObservedAt
	return nil
}
