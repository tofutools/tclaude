package testharness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// PiSim simulates one pi pane (harness.PiMinimumVersion).
//
// It boots FROM THE PRODUCTION LAUNCH STRING, as GeminiSim does: the simulated
// spawner renders harness.Resolve("pi").Spawn.BuildCommand and ParsePiLaunch
// reads it back, so a spawner regression fails inside a daemon flow test.
//
// It owns pi's real on-disk session format (observed at 1.1.0):
//
//	$PI_CODING_AGENT_DIR/sessions/--<cwd, separators as ->--/<ISO time>_<id>.jsonl
//
// a v3 header line followed by tree entries linked by parentId.
//
// Live status comes from tclaude's pi extension, which this sim stands in for:
// when the launch loads an extension and names the hook binary, every pane
// event is turned into the payload piextension/tclaude-pi.ts sends and handed
// to the production decode and dispatch. PiHookPayload is that payload; the
// real-binary fidelity test compares the extension's own output against it.
type PiSim struct {
	ConvID string
	Cwd    string

	agentDir string
	launch   PiLaunch

	mu          sync.Mutex
	path        string
	alive       bool
	working     bool
	buf         strings.Builder
	ccPresses   int
	lastEntry   string
	name        string
	compactions int
	sessionID   string // the tclaude session row id (TCLAUDE_SESSION_ID)
	onRotate    func(old string, sim *PiSim)
	// dispatching counts hook deliveries in progress. tclaude may type into
	// the pane from inside a hook (the title it restores after a session
	// switch); real pi reads that input independently of the callback, so a
	// line submitted meanwhile is deferred until the delivery returns.
	dispatching int
	deferred    []string
	// measured is what the extension attaches to every event once set:
	// tclaude_context, and tclaude_usage when the session has spent.
	measured map[string]any
}

// PiLaunch is one parsed `pi` invocation.
type PiLaunch struct {
	Binary        string
	Env           map[string]string
	Extensions    []string
	SessionID     string
	Session       string // --session: a session file path or id
	Name          string
	Model         string
	Thinking      string
	Tools         string
	InitialPrompt string
	Extra         []string
}

// LoadsStatusExtension reports whether the launch loads an extension and
// names the hook binary, which is what makes a real pane report status.
func (l PiLaunch) LoadsStatusExtension() bool {
	return len(l.Extensions) > 0 && l.Env[harness.PiHookBinEnvVar] != ""
}

// ParsePiLaunch parses a launch the production spawner produced. It models the
// options tclaude emits exactly; `--session` with `--session-id` is refused as
// a launch tclaude must never render.
func ParsePiLaunch(cmd string) (PiLaunch, error) {
	statements, err := copilotSplitStatements(cmd)
	if err != nil {
		return PiLaunch{}, err
	}
	launch := PiLaunch{Env: map[string]string{}}
	var argv []string
	for i, stmt := range statements {
		if len(stmt) == 0 {
			continue
		}
		if stmt[0] == "export" && len(stmt) == 2 {
			if key, value, ok := strings.Cut(stmt[1], "="); ok {
				launch.Env[key] = value
				continue
			}
		}
		if i != len(statements)-1 {
			return PiLaunch{}, fmt.Errorf("pi launch: unexpected statement %q", strings.Join(stmt, " "))
		}
		argv = stmt
	}
	if len(argv) == 0 {
		return PiLaunch{}, fmt.Errorf("pi launch: no command in %q", cmd)
	}
	launch.Binary = argv[0]
	args := argv[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("pi launch: %s needs a value", arg)
			}
			i++
			return args[i], nil
		}
		var target *string
		switch arg {
		case "--":
			launch.InitialPrompt = strings.Join(args[i+1:], " ")
			i = len(args)
			continue
		case "--extension", "-e":
			v, err := value()
			if err != nil {
				return PiLaunch{}, err
			}
			launch.Extensions = append(launch.Extensions, v)
			continue
		case "--session-id":
			target = &launch.SessionID
		case "--session":
			target = &launch.Session
		case "--name", "-n":
			target = &launch.Name
		case "--model":
			target = &launch.Model
		case "--thinking":
			target = &launch.Thinking
		case "--tools", "-t":
			target = &launch.Tools
		default:
			launch.Extra = append(launch.Extra, arg)
			continue
		}
		v, err := value()
		if err != nil {
			return PiLaunch{}, err
		}
		*target = v
	}
	if launch.Session != "" && launch.SessionID != "" {
		return PiLaunch{}, fmt.Errorf("pi launch: --session and --session-id together")
	}
	return launch, nil
}

// PiAgentDirFor returns pi's agent directory under a test HOME.
func PiAgentDirFor(home string) string { return filepath.Join(home, ".pi", "agent") }

// PiSessionDirName is pi's per-cwd session directory name.
func PiSessionDirName(cwd string) string { return harness.PiSessionDirName(cwd) }

func NewPiSim(t *testing.T, agentDir, cwd, cmd string) (*PiSim, error) {
	launch, err := ParsePiLaunch(cmd)
	if err != nil {
		return nil, err
	}
	p := &PiSim{Cwd: cwd, agentDir: agentDir, launch: launch}
	switch {
	case launch.SessionID != "":
		p.ConvID = launch.SessionID
	case launch.Session != "":
		p.ConvID = launch.Session
		if filepath.IsAbs(launch.Session) {
			p.ConvID = piSimIDFromPath(launch.Session)
		}
	default:
		p.ConvID = convops.GenerateUUID()
	}
	t.Cleanup(p.Shutdown)
	return p, nil
}

func piSimIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if _, id, ok := strings.Cut(base, "_"); ok {
		return id
	}
	return base
}

func (p *PiSim) sessionsDir() string { return filepath.Join(p.agentDir, "sessions") }

// Start opens the session the launch names, as pi does: `--session-id`
// creates the session when absent and opens it otherwise; `--session` must
// find an existing one (by path from anywhere, by id in this project).
func (p *PiSim) Start() error {
	p.mu.Lock()
	if p.alive {
		p.mu.Unlock()
		return nil
	}
	existing := ""
	switch {
	case p.launch.Session != "" && filepath.IsAbs(p.launch.Session):
		if _, err := os.Stat(p.launch.Session); err != nil {
			p.mu.Unlock()
			return fmt.Errorf("pi: session file not found: %s", p.launch.Session)
		}
		existing = p.launch.Session
	default:
		matches, _ := filepath.Glob(filepath.Join(p.sessionsDir(), PiSessionDirName(p.Cwd), "*_"+p.ConvID+".jsonl"))
		if len(matches) > 0 {
			existing = matches[0]
		}
	}
	if existing == "" && p.launch.Session != "" {
		p.mu.Unlock()
		return fmt.Errorf("pi: no session found matching %q", p.launch.Session)
	}
	if existing != "" {
		p.path = existing
		p.lastEntry = piSimLastEntry(existing)
	} else if err := p.createLocked(p.ConvID); err != nil {
		p.mu.Unlock()
		return err
	}
	if p.launch.Name != "" {
		p.appendEntryLocked(map[string]any{"type": "session_info", "name": p.launch.Name})
		p.name = p.launch.Name
	}
	p.alive = true
	p.mu.Unlock()
	return p.hook("SessionStart", map[string]any{"source": "startup"})
}

// createLocked writes a fresh session file with pi's v3 header.
func (p *PiSim) createLocked(id string) error {
	dir := filepath.Join(p.sessionsDir(), PiSessionDirName(p.Cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	now := time.Now().UTC()
	p.path = filepath.Join(dir, strings.NewReplacer(":", "-", ".", "-").Replace(now.Format("2006-01-02T15:04:05.000Z"))+"_"+id+".jsonl")
	p.lastEntry = ""
	if err := p.writeLineLocked(map[string]any{
		"type": "session", "version": 3, "id": id,
		"timestamp": now.Format("2006-01-02T15:04:05.000Z"), "cwd": p.Cwd,
	}); err != nil {
		return err
	}
	if p.launch.Model != "" {
		provider, model, _ := strings.Cut(p.launch.Model, "/")
		p.appendEntryLocked(map[string]any{"type": "model_change", "provider": provider, "modelId": model})
	}
	return nil
}

func piSimLastEntry(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	last := ""
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var entry struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Type != "session" && entry.ID != "" {
			last = entry.ID
		}
	}
	return last
}

func (p *PiSim) writeLineLocked(record any) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(raw, '\n'))
	return err
}

// appendEntryLocked appends a tree entry linked to the previous one.
func (p *PiSim) appendEntryLocked(entry map[string]any) {
	var id [4]byte
	_, _ = rand.Read(id[:])
	entry["id"] = hex.EncodeToString(id[:])
	if p.lastEntry == "" {
		entry["parentId"] = nil
	} else {
		entry["parentId"] = p.lastEntry
	}
	entry["timestamp"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	if p.writeLineLocked(entry) == nil {
		p.lastEntry = entry["id"].(string)
	}
}

func (p *PiSim) appendMessageLocked(message map[string]any) {
	message["timestamp"] = time.Now().UnixMilli()
	p.appendEntryLocked(map[string]any{"type": "message", "message": message})
}

// PiHookPayload is the payload tclaude's pi extension sends for one event: the
// identity every event carries, then the event's own fields.
func PiHookPayload(convID, transcriptPath, cwd, model, event string, fields map[string]any) map[string]any {
	payload := map[string]any{
		"session_id":      convID,
		"transcript_path": transcriptPath,
		"cwd":             cwd,
		"hook_event_name": event,
	}
	if model != "" {
		payload["model"] = model
	}
	for k, v := range fields {
		payload[k] = v
	}
	return payload
}

// hook delivers one event the way the extension would, and returns the
// callback's stdout. Nothing is sent when the launch did not load the
// extension: a real pane without it reports no status at all.
func (p *PiSim) hook(event string, fields map[string]any) error {
	_, err := p.hookOutput(event, fields)
	return err
}

func (p *PiSim) hookOutput(event string, fields map[string]any) (string, error) {
	p.mu.Lock()
	if !p.launch.LoadsStatusExtension() {
		p.mu.Unlock()
		return "", nil
	}
	payload := PiHookPayload(p.ConvID, p.path, p.Cwd, p.launch.Model, event, fields)
	for k, v := range p.measured {
		payload[k] = v
	}
	sessionID := p.sessionID
	// The callback runs in the pane, so it sees the pane's environment.
	amb := session.LocalHookAmbient()
	amb.AutoCompactWindow = p.launch.Env[harness.AutoCompactWindowEnvVar]
	p.mu.Unlock()
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	in, err := session.DecodeHookCallbackInput(raw)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.dispatching++
	p.mu.Unlock()
	var out bytes.Buffer
	err = session.DispatchHookEvent(context.Background(), in, sessionID, amb, &out)
	p.mu.Lock()
	p.dispatching--
	var lines []string
	if p.dispatching == 0 {
		lines, p.deferred = p.deferred, nil
	}
	p.mu.Unlock()
	for _, line := range lines {
		p.submit(line)
	}
	return out.String(), err
}

// SubmitLaunchPrompt submits the launch's first turn, as pi does with a
// message argument once the TUI is up.
func (p *PiSim) SubmitLaunchPrompt() {
	p.mu.Lock()
	prompt := p.launch.InitialPrompt
	alive := p.alive
	p.mu.Unlock()
	if alive && prompt != "" {
		p.submitPrompt(prompt)
	}
}

func (p *PiSim) submitPrompt(text string) {
	p.mu.Lock()
	p.appendMessageLocked(map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}})
	p.working = true
	p.mu.Unlock()
	_ = p.hook("UserPromptSubmit", map[string]any{"prompt": text})
}

// UseTool runs one tool call inside the current turn.
func (p *PiSim) UseTool(name string, args map[string]any) error {
	if err := p.hook("PreToolUse", map[string]any{"tool_name": name, "tool_input": args}); err != nil {
		return err
	}
	return p.hook("PostToolUse", map[string]any{"tool_name": name})
}

// Reply ends the current turn with an assistant message; pi then settles.
func (p *PiSim) Reply(text string) error {
	p.mu.Lock()
	provider, model, _ := strings.Cut(p.launch.Model, "/")
	p.appendMessageLocked(map[string]any{
		"role": "assistant", "content": []map[string]string{{"type": "text", "text": text}},
		"provider": provider, "model": model, "stopReason": "stop",
	})
	p.working = false
	p.mu.Unlock()
	return p.hook("Stop", nil)
}

// Measure sets what the extension reports from now on: pi's context usage
// (tokens in context of the model's window) and the session's spend. pi's
// percent is of the model's window; costUSD under a subscription login is
// what-if.
func (p *PiSim) Measure(tokens, window int64, costUSD float64, subscription bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.measured = map[string]any{
		"tclaude_context": map[string]any{
			"tokens": tokens, "context_window": window, "percent": float64(tokens) / float64(window) * 100,
		},
		"tclaude_usage": map[string]any{
			"input_tokens": tokens, "output_tokens": 0, "cost_usd": costUSD, "calls": 1, "subscription": subscription,
		},
	}
}

// OpenDialog parks the pane on a blocking extension dialog of the given kind
// (confirm, select, input, editor, custom).
func (p *PiSim) OpenDialog(kind, title string) error {
	notification := "elicitation_dialog"
	if kind == "confirm" {
		notification = "permission_prompt"
	}
	return p.hook("Notification", map[string]any{"notification_type": notification, "message": title})
}

// AutoCompact runs a threshold compaction inside the current turn and reports
// whether it happened: tclaude may refuse it, as the extension honours.
func (p *PiSim) AutoCompact() (bool, error) {
	out, err := p.hookOutput("PreCompact", map[string]any{"trigger": "auto"})
	if err != nil {
		return false, err
	}
	var decision struct {
		Decision string `json:"decision"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &decision) == nil && decision.Decision == "block" {
		return false, nil
	}
	p.compact()
	return true, p.hook("PostCompact", map[string]any{"trigger": "auto"})
}

func (p *PiSim) compact() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.compactions++
	p.appendEntryLocked(map[string]any{
		"type": "compaction", "summary": "Simulated compaction summary.",
		"firstKeptEntryId": p.lastEntry, "tokensBefore": 1000,
	})
}

// Receive is the send-keys entry point. Text accumulates until Enter.
// Escape aborts a running turn; Ctrl+C clears the editor, and a second
// press on an empty editor quits.
func (p *PiSim) Receive(text string) {
	p.mu.Lock()
	if !p.alive {
		p.mu.Unlock()
		return
	}
	switch text {
	case "Escape":
		working := p.working
		p.working = false
		p.mu.Unlock()
		if working {
			_ = p.hook("Stop", nil)
		}
		return
	case "C-c":
		if p.buf.Len() > 0 {
			p.buf.Reset()
			p.ccPresses = 0
			p.mu.Unlock()
			return
		}
		p.ccPresses++
		quit := p.ccPresses >= 2
		p.mu.Unlock()
		if quit {
			p.quit()
		}
		return
	case "Enter":
	default:
		p.ccPresses = 0
		p.buf.WriteString(strings.TrimSuffix(strings.TrimPrefix(text, "\x1b[200~"), "\x1b[201~"))
		p.mu.Unlock()
		return
	}
	p.ccPresses = 0
	line := strings.TrimSpace(p.buf.String())
	p.buf.Reset()
	if p.dispatching > 0 {
		p.deferred = append(p.deferred, line)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.submit(line)
}

func (p *PiSim) submit(line string) {
	command, arg, _ := strings.Cut(line, " ")
	switch {
	case line == "":
	case command == "/quit":
		p.quit()
	case command == "/name":
		p.mu.Lock()
		p.name = strings.TrimSpace(arg)
		p.appendEntryLocked(map[string]any{"type": "session_info", "name": p.name})
		p.mu.Unlock()
	case command == "/compact":
		_ = p.hook("PreCompact", map[string]any{"trigger": "manual"})
		p.compact()
		_ = p.hook("PostCompact", map[string]any{"trigger": "manual"})
	case command == "/clone", command == "/new":
		p.rotate(command == "/clone")
	default:
		// Any other slash text (a prompt template, a skill) is a model turn,
		// as is plain text; pi's other built-ins are never sent by tclaude.
		p.submitPrompt(line)
	}
}

// rotate moves the pane to a new session in-process, as /clone (copying the
// current branch) and /new (empty) do.
func (p *PiSim) rotate(copyHistory bool) {
	_ = p.hook("SessionEnd", map[string]any{"reason": "clear"})
	p.mu.Lock()
	old, oldPath := p.ConvID, p.path
	id := convops.GenerateUUID()
	if err := p.createLocked(id); err != nil {
		p.mu.Unlock()
		return
	}
	if copyHistory {
		if raw, err := os.ReadFile(oldPath); err == nil {
			lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
			if len(lines) > 1 {
				f, err := os.OpenFile(p.path, os.O_APPEND|os.O_WRONLY, 0o600)
				if err == nil {
					_, _ = f.Write(append(bytes.Join(lines[1:], []byte("\n")), '\n'))
					_ = f.Close()
				}
				p.lastEntry = piSimLastEntry(p.path)
			}
		}
	}
	p.ConvID = id
	onRotate := p.onRotate
	p.mu.Unlock()
	if onRotate != nil {
		onRotate(old, p)
	}
	_ = p.hook("SessionStart", map[string]any{"source": "clear"})
}

func (p *PiSim) quit() {
	_ = p.hook("SessionEnd", map[string]any{"reason": "prompt_input_exit"})
	p.Shutdown()
}

// Path is the current session file.
func (p *PiSim) Path() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.path
}

// Name is the session name /name or --name last set.
func (p *PiSim) Name() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.name
}

// Compactions reports how many compactions ran.
func (p *PiSim) Compactions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.compactions
}

// Launch returns the parsed launch the pane was started from.
func (p *PiSim) Launch() PiLaunch {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.launch
}

func (p *PiSim) IsAlive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.alive
}

func (p *PiSim) Shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.alive = false
	p.working = false
	p.buf.Reset()
}

var _ PaneSim = (*PiSim)(nil)

const piHarnessName = harness.PiName

// PiRegistry indexes PiSims by label and conv-id.
type PiRegistry struct {
	mu       sync.Mutex
	byConvID map[string]*PiSim
	byLabel  map[string]*PiSim
}

func newPiRegistry() *PiRegistry {
	return &PiRegistry{byConvID: map[string]*PiSim{}, byLabel: map[string]*PiSim{}}
}

func (r *PiRegistry) Set(label string, p *PiSim) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if label != "" {
		r.byLabel[label] = p
	}
	r.byConvID[p.ConvID] = p
}

func (r *PiRegistry) GetByConvID(convID string) *PiSim {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byConvID[convID]
}

func (r *PiRegistry) GetByLabel(label string) *PiSim {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byLabel[label]
}

// piSimExtensionPath stands in for the materialized extension: the sim never
// writes the real one, which lives in the user's cache directory.
const piSimExtensionPath = "/pi-sim/tclaude-pi.ts"

// piBuildLaunchCommand renders the PRODUCTION spawner's launch string.
func piBuildLaunchCommand(spec harness.SpawnSpec) (string, error) {
	h, err := harness.Resolve(piHarnessName)
	if err != nil {
		return "", err
	}
	if h.StatusExtension != nil {
		spec.StatusExtensionPath = piSimExtensionPath
		spec.TclaudeExecutable = "/pi-sim/tclaude"
	}
	return h.Spawn.BuildCommand(spec), nil
}

func (s *simSpawner) piSpawnCwd(cwd string) string {
	if cwd != "" {
		return cwd
	}
	cwd = filepath.Join(s.w.HomeDir, "pi-sim-cwd")
	_ = os.MkdirAll(cwd, 0o755)
	return cwd
}

// spawnNewPi is SpawnNew's `--harness pi` branch.
func (s *simSpawner) spawnNewPi(args clcommon.SpawnArgs) error {
	cwd := s.piSpawnCwd(args.Cwd)
	cmd, err := piBuildLaunchCommand(harness.SpawnSpec{
		Cwd: cwd, SessionID: args.SessionID, Name: args.Name, Model: args.Model,
		Effort: args.Effort, InitialPrompt: args.InitialPrompt, ApprovalPolicy: args.Approval,
	})
	if err != nil {
		return err
	}
	return s.startPi(args, args.Label, cwd, cmd, nil)
}

// spawnResumePi is SpawnResume's `--harness pi` branch.
func (s *simSpawner) spawnResumePi(args clcommon.SpawnArgs) error {
	cwd := s.piSpawnCwd(args.Cwd)
	cmd, err := piBuildLaunchCommand(harness.SpawnSpec{
		Cwd: cwd, ResumeID: args.ConvID, Model: args.Model, Effort: args.Effort,
		InitialPrompt: args.InitialPrompt, ApprovalPolicy: args.Approval,
	})
	if err != nil {
		return err
	}
	label := generateResumeLabel()
	if args.Label != "" {
		label = args.Label
	}
	return s.startPi(args, label, cwd, cmd, s.w.Pis.GetByConvID(args.ConvID))
}

func (s *simSpawner) startPi(args clcommon.SpawnArgs, label, cwd, cmd string, existing *PiSim) error {
	sim, err := NewPiSim(s.t, PiAgentDirFor(s.w.HomeDir), cwd, cmd)
	if err != nil {
		return fmt.Errorf("pi launch would not start: %w", err)
	}
	if existing != nil {
		existing.Shutdown()
	}
	// The pane environment session new exports around the command.
	session.ApplyAutoCompactWindowEnv(harness.MustGet(piHarnessName), args.AutoCompactWindow, sim.launch.Env)
	sim.sessionID = label
	sim.onRotate = func(_ string, p *PiSim) { s.w.Pis.Set(label, p) }
	s.w.RecordPiLaunchCommand(sim.ConvID, cmd)
	s.w.RecordSpawnModel(sim.ConvID, args.Model)
	s.w.RecordSpawnEffort(sim.ConvID, args.Effort)
	s.w.RecordSpawnName(sim.ConvID, args.Name)
	s.w.RecordSpawnInitialPrompt(sim.ConvID, args.InitialPrompt)
	s.w.RecordSpawnTrustDir(sim.ConvID, args.TrustDir)
	s.w.RecordSpawnApproval(sim.ConvID, args.Approval)
	s.w.RecordSpawnSandbox(sim.ConvID, args.Sandbox)
	s.w.RecordSpawnSandboxImplementation(sim.ConvID, args.SandboxImplementation)
	s.w.RecordSpawnSandboxPolicy(sim.ConvID, args.EffectiveSandbox)
	if err := saveSessionWithResumeProvenance(&db.SessionRow{
		ID:                       label,
		TmuxSession:              label,
		ConvID:                   sim.ConvID,
		Cwd:                      cwd,
		Status:                   "running",
		Harness:                  piHarnessName,
		SandboxImplementation:    args.SandboxImplementation,
		HarnessBuiltinMode:       launchHarnessBuiltinMode(piHarnessName, args.Sandbox, args.SandboxImplementation),
		HarnessBuiltinModeSource: args.SandboxChosenBy,
		EffectiveSandbox:         args.EffectiveSandbox,
		ApprovalPolicy:           args.Approval,
	}); err != nil {
		return err
	}
	if err := recordLaunchPosture(label, args); err != nil {
		return err
	}
	if s.w.SpawnPaneDiesAtLaunch {
		return nil
	}
	if err := sim.Start(); err != nil {
		return err
	}
	s.w.Tmux.Register(label, cwd, sim)
	s.w.Pis.Set(label, sim)
	sim.SubmitLaunchPrompt()
	return nil
}
