package product

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/internal/backend/client"
)

type backendCall func(context.Context, string, string, any, any) error

// Only a pre-admission write challenge is retried. The marker must be created by
// this client inside its own sandbox, never by the daemon on its behalf.
func callWithDirectoryProof(ctx context.Context, call backendCall, method, path string, body, result any) error {
	err := call(ctx, method, path, body, result)
	var failure *client.Error
	if method != http.MethodPost || !errors.As(err, &failure) || failure.Status != http.StatusForbidden || failure.Code != "write_proof_required" || failure.WriteProof == nil {
		return err
	}
	proof := failure.WriteProof
	token, tokenErr := hex.DecodeString(proof.Token)
	if tokenErr != nil || len(token) != 16 || hex.EncodeToString(token) != proof.Token || proof.Filename != ".tclaude-write-proof-"+proof.Token || len(proof.Directories) == 0 || len(proof.Directories) > 64 {
		return errors.New("invalid backend directory write-proof challenge")
	}
	var request map[string]json.RawMessage
	encoded, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return marshalErr
	}
	if marshalErr = json.Unmarshal(encoded, &request); marshalErr != nil || request == nil {
		return errors.New("directory write proof requires an object request")
	}
	request["write_proof_token"], _ = json.Marshal(proof.Token)
	// Validate the entire challenge before touching any path.
	seen := make(map[string]bool)
	for _, dir := range proof.Directories {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || seen[dir] {
			return errors.New("invalid backend directory write-proof path")
		}
		seen[dir] = true
	}
	var created []string
	defer func() {
		for _, marker := range created {
			_ = os.Remove(marker)
		}
	}()
	for _, dir := range proof.Directories {
		if err := ctx.Err(); err != nil {
			return err
		}
		marker := filepath.Join(dir, proof.Filename)
		file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("cannot prove write access to launch directory %s: %w", dir, err)
		}
		created = append(created, marker)
		if err := file.Close(); err != nil {
			return err
		}
	}
	return call(ctx, method, path, request, result)
}
