package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// TurnForker is the narrow native capability needed to materialize a Codex
// turn-selectable fork before its terminal TUI resumes the new thread.
type TurnForker interface {
	Fork(context.Context, TurnForkRequest) (string, error)
}

type TurnForkRequest struct {
	StateRoot        string
	WorkingDirectory string
	ThreadID         string
	LastTurnID       string
}

type nativeTurnForker struct{ executable string }

func (f nativeTurnForker) Fork(ctx context.Context, request TurnForkRequest) (string, error) {
	if request.StateRoot == "" || request.ThreadID == "" || request.LastTurnID == "" {
		return "", errors.New("codex turn fork requires exact source evidence")
	}
	command := exec.CommandContext(ctx, f.executable, "app-server", "--listen", "stdio://")
	command.Dir = request.WorkingDirectory
	command.Env = append(os.Environ(), "CODEX_HOME="+request.StateRoot)
	stdin, err := command.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := command.Start(); err != nil {
		return "", fmt.Errorf("start Codex app-server fork helper: %w", err)
	}
	stderrDone := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(io.LimitReader(stderr, 64<<10)); stderrDone <- data }()
	defer func() {
		_ = stdin.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
	}()
	encoder := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	if err := encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "tclaude", "title": "tclaude history fork", "version": "1"}}}); err != nil {
		return "", err
	}
	if _, err := readRPCResponse(scanner, 1); err != nil {
		return "", err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return "", err
	}
	params := map[string]any{"threadId": request.ThreadID, "cwd": request.WorkingDirectory, "lastTurnId": request.LastTurnID}
	if err := encoder.Encode(map[string]any{"id": 2, "method": "thread/fork", "params": params}); err != nil {
		return "", err
	}
	result, err := readRPCResponse(scanner, 2)
	if err != nil {
		select {
		case detail := <-stderrDone:
			if len(detail) != 0 {
				return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(detail)))
			}
		default:
		}
		return "", err
	}
	var fork struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &fork); err != nil || fork.Thread.ID == "" || fork.Thread.ID == request.ThreadID {
		return "", errors.New("codex app-server returned invalid fork identity")
	}
	return fork.Thread.ID, nil
}

func readRPCResponse(scanner *bufio.Scanner, expected int) (json.RawMessage, error) {
	for scanner.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil || len(message.ID) == 0 {
			continue
		}
		var id int
		if json.Unmarshal(message.ID, &id) != nil || id != expected {
			continue
		}
		if message.Error != nil {
			return nil, fmt.Errorf("codex app-server error %d: %s", message.Error.Code, message.Error.Message)
		}
		if len(message.Result) == 0 {
			return nil, errors.New("codex app-server response has no result")
		}
		return message.Result, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.ErrUnexpectedEOF
}

var _ TurnForker = nativeTurnForker{}
