package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// piAsker builds the `pi` argv for a one-shot `tclaude ask` turn. Like every
// asker it returns an ARGV, exec'd without a shell, so the question (which
// carries any piped stdin payload) stays one element, behind `--` so a leading
// dash is never read as an option. Every option was observed against the real
// binary at PiMinimumVersion:
//
//   - CAPTURE (spec.Print) is `--print`: the turn runs and pi exits, writing
//     the final assistant text to stdout. A streamed capture (spec.Stream) is
//     `--mode json` instead, pi's JSONL event stream, which piStreamLine reads.
//     pi reads a piped stdin into the first prompt and waits for it to close,
//     so the child's stdin must be a terminal or /dev/null; `tclaude ask` and
//     the daemon's one-shots give a capture /dev/null.
//   - INTERACTIVE is the TUI on the caller's terminal, the question submitted
//     at launch.
//   - A fresh thread is `--session-id <id>` (pi creates it under that id); a
//     resume is `--session <file>`, the session file resolved as the spawner
//     resolves it, since by id pi offers to fork a session another working
//     directory owns.
//
// pi never asks before a tool call, so the posture is tclaude's to set:
//
//   - A capture with no recorded posture gets pi's read-only tools: nobody
//     can approve anything in a capture, the same outcome as Claude Code's
//     headless default.
//   - An interactive ask loads tclaude's permission extension (the `ask`
//     posture): the human at the terminal approves each acting tool call.
//   - A brokered replay (LaunchPosture: a séance, auto-naming) renders the
//     recorded approval posture. `ask` replays as read-only, since a capture
//     has no human to ask; anything unrecognised is read-only too.
//
// Ephemeral: a fresh ephemeral turn is `--no-session`. A resumed one continues
// a copy of the session file (EphemeralResumer), which pi appends to and the
// caller removes; one without the copy gets no argv, so it fails closed
// instead of appending to the original.
type piAsker struct{}

var (
	_ StreamAsker      = piAsker{}
	_ EphemeralResumer = piAsker{}
)

func (piAsker) BuildAskArgv(spec AskSpec) []string {
	if spec.Ephemeral && spec.ResumeID != "" && spec.ResumeFile == "" {
		return nil
	}
	argv := []string{"pi"}
	if spec.Print {
		if spec.Stream {
			argv = append(argv, "--mode", "json")
		} else {
			argv = append(argv, "--print")
		}
	}
	switch {
	case spec.ResumeFile != "":
		argv = append(argv, "--session", spec.ResumeFile)
	case spec.ResumeID != "":
		session := spec.ResumeID
		if path := piSessionFileLookup(piAskCwd(spec), spec.ResumeID); path != "" {
			session = path
		}
		argv = append(argv, "--session", session)
	case spec.Ephemeral && spec.Print:
		argv = append(argv, "--no-session")
	case spec.SessionID != "":
		argv = append(argv, "--session-id", spec.SessionID)
	}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Effort != "" {
		argv = append(argv, "--thinking", spec.Effort)
	}
	argv = append(argv, piAskPostureArgs(spec)...)
	if spec.Prompt != "" {
		argv = append(argv, "--", spec.Prompt)
	}
	return argv
}

// piAskCwd is the directory the ask runs in: the recorded launch's for a
// brokered replay, the caller's otherwise.
func piAskCwd(spec AskSpec) string {
	if spec.LaunchPosture != nil && spec.LaunchPosture.Cwd != "" {
		return spec.LaunchPosture.Cwd
	}
	cwd, _ := os.Getwd()
	return cwd
}

func piAskPostureArgs(spec AskSpec) []string {
	readOnly := []string{"--tools", strings.Join(piReadOnlyTools, ",")}
	if !spec.Print {
		// The human at the terminal answers the permission extension.
		status, err := MaterializePiExtension()
		if err != nil {
			return readOnly
		}
		return []string{"--extension", PiPermissionExtensionPath(status)}
	}
	if posture := spec.LaunchPosture; posture != nil &&
		strings.TrimSpace(posture.ApprovalPolicy) == PiApprovalAuto {
		return nil
	}
	return readOnly
}

// PreMintsConvID is true: `--session-id <id>` creates the session under the
// caller's id in print mode too, so later asks resume that exact id.
func (piAsker) PreMintsConvID() bool { return true }

// NoisyCaptureStderr is true: pi writes warnings (an extension that failed to
// load, a model it fell back from) to stderr, which `tclaude ask` keeps for
// --verbose or a failed run.
func (piAsker) NoisyCaptureStderr() bool { return true }

// StreamFilter renders pi's `--mode json` event stream as the answer's text.
func (piAsker) StreamFilter(w io.Writer, smooth bool, status StreamStatus) io.Writer {
	return newPacedStreamFilter(w, smooth, status, piStreamLine)
}

// piStreamLine reads one line of pi's JSONL event stream (docs/json.md):
// assistantMessageEvent text_start opens a text section and text_delta carries
// its text; thinking and tool-call deltas are never shown. An assistant
// message_end is the fallback answer, shown only when nothing streamed: its
// text, or its errorMessage when the provider failed.
func piStreamLine(line []byte) (streamLineKind, string) {
	var ev struct {
		Type  string `json:"type"`
		Event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		} `json:"assistantMessageEvent"`
		Message struct {
			Role         string          `json:"role"`
			Content      json.RawMessage `json:"content"`
			ErrorMessage string          `json:"errorMessage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return streamLineIgnored, ""
	}
	switch ev.Type {
	case "message_update":
		switch ev.Event.Type {
		case "text_start":
			return streamLineSection, ""
		case "text_delta":
			return streamLineDelta, ev.Event.Delta
		}
	case "message_end":
		if ev.Message.Role != "assistant" {
			break
		}
		if ev.Message.ErrorMessage != "" {
			return streamLineResult, ev.Message.ErrorMessage
		}
		if text := piContentText(ev.Message.Content); text != "" {
			return streamLineResult, text
		}
	}
	return streamLineIgnored, ""
}

// piSeanceDirName holds ephemeral resume copies in pi's session store, which
// tclaude's sandbox leaves writable. Two levels below the store's per-project
// directories, neither pi's session list nor tclaude's conversation scan
// (piSessionFiles) sees them.
const piSeanceDirName = "tclaude-seance"

// PrepareEphemeralResume copies the session file holding convID, so a resumed
// one-shot continues the copy and leaves the conversation as it was.
func (piAsker) PrepareEphemeralResume(convID string) (string, func(), error) {
	noop := func() {}
	source := ""
	for _, root := range piSessionRoots() {
		if source = piFindSessionFile(root, convID); source != "" {
			break
		}
	}
	if source == "" {
		return "", noop, fmt.Errorf("pi: no session file holds conversation %s", convID)
	}
	agentDir, err := piAgentDir(nil)
	if err != nil {
		return "", noop, err
	}
	parent := filepath.Join(agentDir, "sessions", piSeanceDirName)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", noop, err
	}
	// The session store is writable to a walled pi, which could plant this
	// directory as a symlink to steer where the daemon writes.
	if info, err := os.Lstat(parent); err != nil || !info.IsDir() {
		return "", noop, fmt.Errorf("pi: %s is not a plain directory", parent)
	}
	dir, err := os.MkdirTemp(parent, "resume-")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	copyPath := filepath.Join(dir, filepath.Base(source))
	if err := copyPiSessionFile(source, copyPath); err != nil {
		cleanup()
		return "", noop, err
	}
	return copyPath, cleanup, nil
}

func copyPiSessionFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return errors.Join(out.Close())
}
