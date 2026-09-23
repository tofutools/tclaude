package agentd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestOneShotExecHelperReturnsResultFromPrivateHandoff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(config.DataDir(), "one-shot")
	dir := filepath.Join(root, "run-test")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(dir, "request.json")
	resultPath := filepath.Join(dir, "result.json")
	request := nonInteractiveBrokerRequest{
		Command: nonInteractiveCommand{
			Argv: []string{"/bin/sh", "-c", "printf 'broker-ok\\n'; exit 7"},
			Cwd:  t.TempDir(), Env: os.Environ(), TimeoutSeconds: 10,
		},
		Deadline: time.Now().Add(10 * time.Second),
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runOneShotExecHelper(requestPath, resultPath); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var reply nonInteractiveBrokerReply
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Failure != nil || reply.Result.Stdout != "broker-ok\n" || reply.Result.ExitCode != 7 {
		t.Fatalf("unexpected helper reply: %+v", reply)
	}
}

func TestOneShotExecHelperRefusesHandoffOutsidePrivateData(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	outside := filepath.Join(t.TempDir(), "request.json")
	result := filepath.Join(t.TempDir(), "result.json")
	if err := runOneShotExecHelper(outside, result); err == nil ||
		!strings.Contains(err.Error(), "private data directory") {
		t.Fatalf("expected private handoff refusal, got %v", err)
	}
}
