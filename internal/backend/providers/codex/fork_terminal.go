package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/tofutools/tclaude/internal/backend/host"
)

// ForkTerminalCommand runs inside the already-selected host boundary. The
// terminal supervises this process throughout fork preparation and the final
// exec; recovery must observe it rather than creating a second native fork.
const ForkTerminalCommand = "__codex-fork-terminal"

// ForkTerminalRequest is a provider-created command, never an API DTO. Args
// contain the ordinary resume command with the exact source ID in slot six.
// Only that slot is replaced by the native helper's returned conversation ID.
type ForkTerminalRequest struct {
	Executable string
	Fork       TurnForkRequest
	Args       []string
	Receipt    string
}

func ExecuteForkTerminal(ctx context.Context, request ForkTerminalRequest) error {
	return executeForkTerminal(ctx, request, func(executable string, args, env []string) error {
		if err := os.Chdir(request.Fork.WorkingDirectory); err != nil {
			return err
		}
		return syscall.Exec(executable, args, env)
	})
}

func executeForkTerminal(ctx context.Context, request ForkTerminalRequest, replace func(string, []string, []string) error) error {
	if !filepath.IsAbs(request.Executable) || !filepath.IsAbs(request.Fork.StateRoot) || !filepath.IsAbs(request.Fork.WorkingDirectory) || !filepath.IsAbs(request.Receipt) || len(request.Args) < 7 || request.Args[5] != "resume" || request.Args[6] != request.Fork.ThreadID {
		return errors.New("invalid confined Codex fork command")
	}
	// Fence the helper itself as well as the outer sandbox artifact. A failed
	// or interrupted helper may already have produced a native conversation.
	started, err := os.OpenFile(request.Receipt+".started", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("claim confined Codex fork: %w", err)
	}
	err = started.Sync()
	err = errors.Join(err, started.Close())
	if err != nil {
		return err
	}
	nativeID, err := (nativeTurnForker{executable: request.Executable}).Fork(ctx, request.Fork)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(nativeID)
	if err != nil {
		return err
	}
	if err := host.WriteProtectedFile(request.Receipt, raw); err != nil {
		return err
	}
	args := append([]string(nil), request.Args...)
	args[6] = nativeID
	return replace(request.Executable, append([]string{request.Executable}, args...), os.Environ())
}

func readForkReceipt(path string) (string, error) {
	raw, err := host.ReadProtectedFile(path, 4096)
	if err != nil {
		return "", err
	}
	var nativeID string
	if json.Unmarshal(raw, &nativeID) != nil || nativeID == "" {
		return "", errors.New("invalid confined Codex fork receipt")
	}
	return nativeID, nil
}
