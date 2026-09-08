package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
)

// ServerRelayImport is retained in the launch artifact. The input is a private,
// read-only resource; native import and verification run inside the same boundary
// as the server, after the artifact's one-shot release marker has been claimed.
type ServerRelayImport struct {
	Path          string
	Digest        string
	NativeID      string
	WorkingDir    string
	BeforeMessage string
}

func importRelayHistory(ctx context.Context, executable string, selected ServerRelayImport) error {
	if !filepath.IsAbs(selected.Path) || !filepath.IsAbs(selected.WorkingDir) ||
		!strings.HasPrefix(selected.NativeID, "ses_") {
		return fmt.Errorf("invalid retained OpenCode import")
	}
	expectedDigest, err := hex.DecodeString(selected.Digest)
	if err != nil || len(expectedDigest) != sha256.Size {
		return fmt.Errorf("invalid retained OpenCode import digest")
	}
	raw, err := os.ReadFile(selected.Path)
	if err != nil {
		return fmt.Errorf("read retained OpenCode import: %w", err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != selected.Digest {
		return fmt.Errorf("retained OpenCode import changed")
	}
	var expected exportedHistory
	if err := json.Unmarshal(raw, &expected); err != nil || expected.Info.ID != selected.NativeID {
		return fmt.Errorf("retained OpenCode import does not match selected history")
	}
	// Check the selected point before any native effect as well as after import.
	if err := verifyImportedHistory(expected, expected, selected.BeforeMessage); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, executable, "import", selected.Path, "--pure")
	command.Dir = selected.WorkingDir
	// Inherit the exact environment already installed by the sandbox bootstrap.
	// Do not include native output (which may contain conversation text) in errors.
	if err := command.Run(); err != nil {
		return fmt.Errorf("import retained OpenCode history: %w", err)
	}
	command = exec.CommandContext(ctx, executable, "export", selected.NativeID, "--pure")
	command.Dir = selected.WorkingDir
	raw, err = command.Output()
	if err != nil {
		return fmt.Errorf("verify retained OpenCode import: %w", err)
	}
	var imported exportedHistory
	if err := json.Unmarshal(raw, &imported); err != nil ||
		filepath.Clean(imported.Info.Directory) != filepath.Clean(selected.WorkingDir) {
		return fmt.Errorf("imported OpenCode history does not match working directory")
	}
	return verifyImportedHistory(imported, expected, selected.BeforeMessage)
}

func verifyImportedHistory(imported, expected exportedHistory, beforeMessage string) error {
	if imported.Info.ID != expected.Info.ID || !reflect.DeepEqual(imported.Messages, expected.Messages) {
		return fmt.Errorf("imported OpenCode history content does not match selected source")
	}
	if beforeMessage != "" {
		for _, message := range imported.Messages {
			if message.Info.ID == beforeMessage {
				return nil
			}
		}
		return fmt.Errorf("imported OpenCode history point is unavailable")
	}
	return nil
}
