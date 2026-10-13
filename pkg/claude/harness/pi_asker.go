package harness

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
// A brokered replay also ignores project-local resources (`--no-approve`):
// see BuildAskArgv.
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
	if spec.LaunchPosture != nil {
		// A brokered one-shot (a séance, auto-naming) is the daemon's run,
		// started at an agent's request in the agent's own directory. pi's
		// project trust there is the operator's, so without this a project
		// extension the agent wrote would run as the operator, read-only
		// tools or not.
		argv = append(argv, "--no-approve")
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
	sessions := filepath.Join(agentDir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		return "", noop, err
	}
	// The session store is writable to a walled pi, which could plant the
	// copy's directories as symlinks to steer where the daemon writes or
	// deletes. Every step goes through a root on the store, which refuses a
	// path that leaves it.
	store, err := os.OpenRoot(sessions)
	if err != nil {
		return "", noop, err
	}
	defer func() { _ = store.Close() }()
	if err := store.Mkdir(piSeanceDirName, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", noop, err
	}
	dir := filepath.Join(piSeanceDirName, "resume-"+rand.Text())
	if err := store.Mkdir(dir, 0o700); err != nil {
		return "", noop, err
	}
	cleanup := func() {
		if store, err := os.OpenRoot(sessions); err == nil {
			_ = store.RemoveAll(dir)
			_ = store.Close()
		}
	}
	copyName := filepath.Join(dir, filepath.Base(source))
	if err := copyPiSessionFile(source, store, copyName); err != nil {
		cleanup()
		return "", noop, err
	}
	return filepath.Join(sessions, copyName), cleanup, nil
}

// openSeanceSource opens a session file a séance copies: a regular file with
// one link, never a symlink or hard link an agent planted in a store it can
// write, to have the daemon copy a file the sandbox hides out into the store.
func openSeanceSource(path string) (*os.File, error) {
	file, info, err := openRegularFile(path, false)
	if err != nil {
		return nil, err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && uint64(stat.Nlink) != 1 {
		_ = file.Close()
		return nil, fmt.Errorf("session file %s has %d links", path, stat.Nlink)
	}
	return file, nil
}

// copyPiSessionFile copies the session file into the store.
func copyPiSessionFile(source string, store *os.Root, target string) error {
	in, err := openSeanceSource(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := store.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
