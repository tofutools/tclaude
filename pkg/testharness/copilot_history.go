package testharness

import (
	"encoding/json"
	"github.com/google/uuid"
	"os"
	"path/filepath"
	"time"
)

// AppendCopilotHistory models the native append-only turn completion boundary.
func AppendCopilotHistory(home, id, prompt, answer string) error {
	f, err := os.OpenFile(filepath.Join(CopilotHomeFor(home), "session-state", id, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	encode := json.NewEncoder(f)
	for _, record := range []struct{ kind, text string }{{"user.message", prompt}, {"assistant.message", answer}} {
		if err = encode.Encode(map[string]any{"id": uuid.NewString(), "type": record.kind, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "data": map[string]string{"content": record.text}}); err != nil {
			return err
		}
	}
	return nil
}
