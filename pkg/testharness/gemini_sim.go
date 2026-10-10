package testharness

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// GeminiSim simulates one Gemini CLI pane (GeminiPinnedVersion), the Gemini
// analog of CopilotSim.
//
// Like the Copilot branch it boots FROM THE PRODUCTION LAUNCH STRING: the
// simulated spawner renders harness.Resolve("gemini").Spawn.BuildCommand and
// ParseGeminiLaunch reads it back, so a spawner regression (a respelt flag, a
// dash-led value yargs would not bind, --resume combined with --session-id)
// fails inside a daemon flow test at the boundary where the real CLI would
// have refused it.
//
// It owns Gemini's real on-disk conversation format, so the production
// ConvStore reads it unchanged:
//
//	$HOME/.gemini/projects.json
//	$HOME/.gemini/tmp/<slug>/.project_root
//	$HOME/.gemini/tmp/<slug>/chats/session-<YYYY-MM-DDTHH-MM>-<id[:8]>.jsonl
//
// Faithful-but-minimal in the same way the other sims are: it writes the user
// side of a turn (which is what makes a session resumable in Gemini's eyes)
// and never fabricates assistant output; tests that need a reply call
// WriteGeminiReply.
type GeminiSim struct {
	ConvID string
	Cwd    string

	home   string // the HOME Gemini resolves (GEMINI_CLI_HOME is never set here)
	launch GeminiLaunch

	mu           sync.Mutex
	path         string
	alive        bool
	buf          strings.Builder
	ccPresses    int
	nextMsg      int
	compressions int
	sessionID    string // the tclaude session row id (TCLAUDE_SESSION_ID)
}

// GeminiLaunch is one parsed `gemini` invocation.
type GeminiLaunch struct {
	Binary string
	Env    map[string]string
	// Unset names the variables the launch line removes from the inherited
	// environment (after any export of the same name).
	Unset         map[string]bool
	SessionID     string
	ResumeID      string
	Model         string
	InitialPrompt string
	// ApprovalMode is the rendered `--approval-mode=` value; "" when none.
	ApprovalMode string
	// Extra holds pass-through arguments the parser does not model.
	Extra []string
}

var geminiSimIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ParseGeminiLaunch parses a launch the production spawner produced. It pins
// what tclaude may EMIT rather than reimplementing yargs: the option spellings
// tclaude renders are modelled exactly, and the two constraints the real CLI
// enforces before starting (mutually exclusive session options; a safe
// session id) are parse errors here.
func ParseGeminiLaunch(cmd string) (GeminiLaunch, error) {
	statements, err := copilotSplitStatements(cmd)
	if err != nil {
		return GeminiLaunch{}, err
	}
	launch := GeminiLaunch{Env: map[string]string{}, Unset: map[string]bool{}}
	var argv []string
	for i, stmt := range statements {
		if len(stmt) == 0 {
			continue
		}
		if stmt[0] == "export" && len(stmt) == 2 {
			if key, value, ok := strings.Cut(stmt[1], "="); ok {
				launch.Env[key] = value
				delete(launch.Unset, key)
				continue
			}
		}
		if stmt[0] == "unset" && len(stmt) >= 2 {
			for _, key := range stmt[1:] {
				delete(launch.Env, key)
				launch.Unset[key] = true
			}
			continue
		}
		if i != len(statements)-1 {
			return GeminiLaunch{}, fmt.Errorf("gemini launch: unexpected statement %q", strings.Join(stmt, " "))
		}
		argv = stmt
	}
	if len(argv) == 0 {
		return GeminiLaunch{}, fmt.Errorf("gemini launch: no command in %q", cmd)
	}
	launch.Binary = argv[0]
	args := argv[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				// yargs nargs:1 refuses a dash-led value: "Not enough
				// arguments following: <option>".
				return "", fmt.Errorf("gemini launch: Not enough arguments following: %s", arg)
			}
			i++
			return args[i], nil
		}
		switch {
		case arg == "--resume" || arg == "-r":
			v, err := value()
			if err != nil {
				return GeminiLaunch{}, err
			}
			launch.ResumeID = v
		case arg == "--session-id":
			v, err := value()
			if err != nil {
				return GeminiLaunch{}, err
			}
			launch.SessionID = v
		case strings.HasPrefix(arg, "--model="):
			launch.Model = strings.TrimPrefix(arg, "--model=")
		case arg == "--model" || arg == "-m":
			v, err := value()
			if err != nil {
				return GeminiLaunch{}, err
			}
			launch.Model = v
		case strings.HasPrefix(arg, "--approval-mode="):
			launch.ApprovalMode = strings.TrimPrefix(arg, "--approval-mode=")
		case strings.HasPrefix(arg, "--prompt-interactive="):
			launch.InitialPrompt = strings.TrimPrefix(arg, "--prompt-interactive=")
		case arg == "--prompt-interactive" || arg == "-i":
			v, err := value()
			if err != nil {
				return GeminiLaunch{}, err
			}
			launch.InitialPrompt = v
		default:
			launch.Extra = append(launch.Extra, arg)
		}
	}
	if launch.ResumeID != "" && launch.SessionID != "" {
		return GeminiLaunch{}, errors.New("gemini launch: The flags --resume, --session-id, and --session-file are mutually exclusive")
	}
	if launch.SessionID != "" && !geminiSimIDRE.MatchString(launch.SessionID) {
		return GeminiLaunch{}, fmt.Errorf("gemini launch: Invalid session ID %q", launch.SessionID)
	}
	return launch, nil
}

// NewGeminiSim builds a pane from a production launch string. A fresh launch
// takes its id from --session-id (Gemini mints one when absent); a resume
// takes the resumed id. Inert until Start.
func NewGeminiSim(t *testing.T, home, cwd, cmd string) (*GeminiSim, error) {
	launch, err := ParseGeminiLaunch(cmd)
	if err != nil {
		return nil, err
	}
	convID := launch.ResumeID
	if convID == "" {
		convID = launch.SessionID
	}
	if convID == "" {
		convID = convops.GenerateUUID()
	}
	g := &GeminiSim{ConvID: convID, Cwd: cwd, home: home, launch: launch}
	t.Cleanup(g.Shutdown)
	return g, nil
}

// GeminiDirFor returns the .gemini directory under a test HOME.
func GeminiDirFor(home string) string { return filepath.Join(home, ".gemini") }

// Start opens the conversation the launch names, the way gemini.tsx
// resolveSessionId does: a resume must find an existing resumable session in
// the cwd's project (otherwise the CLI exits with "Invalid session
// identifier"), and a fresh --session-id must NOT already exist.
func (g *GeminiSim) Start() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.alive {
		return nil
	}
	slugDir, err := geminiSimProjectDir(g.home, g.Cwd)
	if err != nil {
		return err
	}
	chats := filepath.Join(slugDir, "chats")
	existing := geminiSimFindSession(chats, g.ConvID)
	if g.launch.ResumeID != "" {
		if existing == "" {
			return fmt.Errorf("gemini: Error resuming session: Invalid session identifier %q", g.ConvID)
		}
		g.path = existing
	} else {
		if existing != "" {
			return fmt.Errorf("gemini: Error starting session: Session ID %q already exists", g.ConvID)
		}
		if err := os.MkdirAll(chats, 0o755); err != nil {
			return err
		}
		now := time.Now().UTC()
		g.path = filepath.Join(chats, fmt.Sprintf("session-%s-%s.jsonl",
			strings.ReplaceAll(now.Format("2006-01-02T15:04"), ":", "-"), g.ConvID[:min(8, len(g.ConvID))]))
		if err := g.appendLocked(map[string]any{
			"sessionId":   g.ConvID,
			"projectHash": "sim",
			"startTime":   now.Format(time.RFC3339Nano),
			"lastUpdated": now.Format(time.RFC3339Nano),
			"kind":        "main",
		}); err != nil {
			return err
		}
	}
	g.alive = true
	return nil
}

// geminiSimProjectDir returns (creating if needed) the tmp/<slug> directory for
// a project root, registering it the way ProjectRegistry does.
func geminiSimProjectDir(home, cwd string) (string, error) {
	dir := GeminiDirFor(home)
	registryPath := filepath.Join(dir, "projects.json")
	registry := struct {
		Projects map[string]string `json:"projects"`
	}{Projects: map[string]string{}}
	if raw, err := os.ReadFile(registryPath); err == nil {
		_ = json.Unmarshal(raw, &registry)
		if registry.Projects == nil {
			registry.Projects = map[string]string{}
		}
	}
	// Gemini keys the project by process.cwd(), which the kernel reports with
	// symlinks resolved (macOS's /var → /private/var), so a spawn and a resume
	// that spell the same directory differently land in one project.
	root := filepath.Clean(cwd)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	slug, ok := registry.Projects[root]
	if !ok {
		// Native ProjectRegistry adopts a matching ownership marker before
		// inventing a new slug (for stores imported before the first launch).
		dirs, _ := os.ReadDir(filepath.Join(dir, "tmp"))
		for _, entry := range dirs {
			if !entry.IsDir() {
				continue
			}
			marker, e := os.ReadFile(filepath.Join(dir, "tmp", entry.Name(), ".project_root"))
			if e == nil && strings.TrimSpace(string(marker)) == root {
				slug = entry.Name()
				ok = true
				break
			}
		}
	}
	if !ok {
		base := strings.ToLower(regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(
			strings.ToLower(filepath.Base(root)), "-"))
		if base == "" {
			base = "project"
		}
		taken := map[string]bool{}
		for _, s := range registry.Projects {
			taken[s] = true
		}
		slug = base
		for n := 1; taken[slug]; n++ {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
	}
	if registry.Projects[root] != slug {
		registry.Projects[root] = slug
		raw, _ := json.MarshalIndent(registry, "", "  ")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(registryPath, raw, 0o644); err != nil {
			return "", err
		}
	}
	slugDir := filepath.Join(dir, "tmp", slug)
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(slugDir, ".project_root"), []byte(root), 0o644); err != nil {
		return "", err
	}
	return slugDir, nil
}

// geminiSimFindSession returns the session file in chats whose metadata names
// convID AND which holds a resumable user turn — the same rule --resume uses.
func geminiSimFindSession(chats, convID string) string {
	files, _ := os.ReadDir(chats)
	for _, f := range files {
		if f.IsDir() || !strings.HasPrefix(f.Name(), "session-") {
			continue
		}
		path := filepath.Join(chats, f.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(raw), "\n")
		if !strings.Contains(lines[0], `"sessionId":"`+convID+`"`) {
			continue
		}
		for _, line := range lines[1:] {
			if strings.Contains(line, `"type":"user"`) {
				return path
			}
		}
	}
	return ""
}

func (g *GeminiSim) appendLocked(record any) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(g.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(raw, '\n'))
	return err
}

func (g *GeminiSim) writeUserTurnLocked(text string) {
	g.nextMsg++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_ = g.appendLocked(map[string]any{
		"id": fmt.Sprintf("u-%d", g.nextMsg), "timestamp": now,
		"type": "user", "content": []map[string]string{{"text": text}},
	})
	_ = g.appendLocked(map[string]any{"$set": map[string]string{"lastUpdated": now}})
}

// SubmitLaunchPrompt lands the launch's first turn, as the TUI's initial-prompt
// effect does once the client is up.
func (g *GeminiSim) SubmitLaunchPrompt() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.alive && g.launch.InitialPrompt != "" {
		g.writeUserTurnLocked(g.launch.InitialPrompt)
	}
}

// WriteGeminiReply appends an assistant turn, for tests that need one.
func (g *GeminiSim) WriteGeminiReply(text, model string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextMsg++
	_ = g.appendLocked(map[string]any{
		"id": fmt.Sprintf("g-%d", g.nextMsg), "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"type": "gemini", "content": text, "model": model,
	})
}

// WriteGeminiReplyWithTokens appends a model reply carrying the usage record
// chatRecordingService.recordMessageTokens stamps on it.
func (g *GeminiSim) WriteGeminiReplyWithTokens(text, model string, input, output, thoughts int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextMsg++
	_ = g.appendLocked(map[string]any{
		"id": fmt.Sprintf("g-%d", g.nextMsg), "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"type": "gemini", "content": text, "model": model,
		"tokens": map[string]int64{
			"input": input, "output": output, "cached": 0, "thoughts": thoughts,
			"tool": 0, "total": input + output + thoughts,
		},
	})
}

// SetSummary writes Gemini's own generated summary for the session.
func (g *GeminiSim) SetSummary(summary string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_ = g.appendLocked(map[string]any{"$set": map[string]string{"summary": summary}})
}

// Receive is the send-keys entry point. Text accumulates until Enter; a
// submitted line is a slash command or a user turn. Ctrl+C cancels and counts
// toward the double-press quit (AppContainer's 3 s window is not timed here);
// any other key resets the count.
func (g *GeminiSim) Receive(text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.alive {
		return
	}
	switch text {
	case "C-c":
		g.buf.Reset()
		g.ccPresses++
		if g.ccPresses >= 2 {
			g.alive = false
		}
		return
	case "Escape":
		return
	case "Enter":
	default:
		g.ccPresses = 0
		g.buf.WriteString(text)
		return
	}
	g.ccPresses = 0
	line := strings.TrimSpace(g.buf.String())
	g.buf.Reset()
	switch line {
	case "":
	case "/quit", "/exit":
		g.alive = false
	case "/compress", "/compact", "/summarize":
		g.compressions++
		_ = g.appendLocked(map[string]any{
			"id": fmt.Sprintf("i-%d", g.compressions), "type": "info",
			"content": "Chat history compressed.",
		})
	default:
		if !strings.HasPrefix(line, "/") {
			g.writeUserTurnLocked(line)
		}
	}
}

// FireHook delivers one Gemini-shaped hook payload to the production callback
// — session.DecodeHookCallbackInput (which translates Gemini's event names)
// then session.ApplyHook — the way a settings.json hook would. The base
// fields every Gemini hook carries are filled in; fields adds the per-event
// ones (prompt, prompt_response, tool_name, notification_type, …).
//
// Explicit rather than automatic: a real pane fires hooks only once they are
// installed AND the folder is trusted, which a test states by calling this.
// The lock is not held across ApplyHook (see CopilotSim.applyHook).
func (g *GeminiSim) FireHook(event string, fields map[string]any) error {
	g.mu.Lock()
	payload := map[string]any{
		"session_id":      g.ConvID,
		"transcript_path": g.path,
		"cwd":             g.Cwd,
		"hook_event_name": event,
		"timestamp":       time.Now().UTC().Format(time.RFC3339Nano),
	}
	sessionID := g.sessionID
	g.mu.Unlock()
	for k, v := range fields {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	in, err := session.DecodeHookCallbackInput(raw)
	if err != nil {
		return err
	}
	return session.ApplyHook(in, sessionID)
}

// Compressions reports how many /compress commands the pane accepted.
func (g *GeminiSim) Compressions() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.compressions
}

// Launch returns the parsed launch the pane was started from.
func (g *GeminiSim) Launch() GeminiLaunch {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.launch
}

func (g *GeminiSim) IsAlive() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.alive
}

func (g *GeminiSim) Shutdown() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.alive = false
	g.buf.Reset()
}

var _ PaneSim = (*GeminiSim)(nil)

const geminiHarnessName = harness.GeminiName

// GeminiRegistry indexes GeminiSims by label and conv-id.
type GeminiRegistry struct {
	mu       sync.Mutex
	byConvID map[string]*GeminiSim
	byLabel  map[string]*GeminiSim
}

func newGeminiRegistry() *GeminiRegistry {
	return &GeminiRegistry{byConvID: map[string]*GeminiSim{}, byLabel: map[string]*GeminiSim{}}
}

func (r *GeminiRegistry) Set(label string, g *GeminiSim) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if label != "" {
		r.byLabel[label] = g
	}
	r.byConvID[g.ConvID] = g
}

func (r *GeminiRegistry) GetByConvID(convID string) *GeminiSim {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byConvID[convID]
}

// geminiBuildLaunchCommand renders the PRODUCTION spawner's launch string.
func geminiBuildLaunchCommand(spec harness.SpawnSpec) (string, error) {
	h, err := harness.Resolve(geminiHarnessName)
	if err != nil {
		return "", err
	}
	return h.Spawn.BuildCommand(spec), nil
}

func (s *simSpawner) geminiSpawnCwd(cwd string) string {
	if cwd != "" {
		return cwd
	}
	cwd = filepath.Join(s.w.HomeDir, "gemini-sim-cwd")
	_ = os.MkdirAll(cwd, 0o755)
	return cwd
}

// spawnNewGemini is SpawnNew's `--harness gemini` branch.
func (s *simSpawner) spawnNewGemini(args clcommon.SpawnArgs) error {
	cwd := s.geminiSpawnCwd(args.Cwd)
	cmd, err := geminiBuildLaunchCommand(harness.SpawnSpec{
		Cwd: cwd, SessionID: args.SessionID, Name: args.Name, Model: args.Model,
		Effort: args.Effort, InitialPrompt: args.InitialPrompt, HarnessBuiltinMode: launchHarnessBuiltinMode(geminiHarnessName, args.Sandbox, args.SandboxImplementation),
		ApprovalPolicy: args.Approval,
	})
	if err != nil {
		return err
	}
	return s.startGemini(args, args.Label, cwd, cmd, nil)
}

// spawnResumeGemini is SpawnResume's `--harness gemini` branch. A resume
// reopens and appends to the same session file, so a fresh sim over that file
// replaces any earlier pane for the conv.
func (s *simSpawner) spawnResumeGemini(args clcommon.SpawnArgs) error {
	cwd := s.geminiSpawnCwd(args.Cwd)
	cmd, err := geminiBuildLaunchCommand(harness.SpawnSpec{
		Cwd: cwd, ResumeID: args.ConvID, Model: args.Model, Effort: args.Effort,
		InitialPrompt: args.InitialPrompt, HarnessBuiltinMode: launchHarnessBuiltinMode(geminiHarnessName, args.Sandbox, args.SandboxImplementation),
		ApprovalPolicy: args.Approval,
	})
	if err != nil {
		return err
	}
	label := generateResumeLabel()
	if args.Label != "" {
		label = args.Label
	}
	return s.startGemini(args, label, cwd, cmd, s.w.Geminis.GetByConvID(args.ConvID))
}

func (s *simSpawner) startGemini(args clcommon.SpawnArgs, label, cwd, cmd string, existing *GeminiSim) error {
	sim, err := NewGeminiSim(s.t, s.w.HomeDir, cwd, cmd)
	if err != nil {
		return fmt.Errorf("gemini launch would not start: %w", err)
	}
	if existing != nil {
		existing.Shutdown()
	}
	sim.sessionID = label
	s.w.RecordGeminiLaunchCommand(sim.ConvID, cmd)
	if err := sim.Start(); err != nil {
		return err
	}
	s.w.RecordSpawnModel(sim.ConvID, args.Model)
	s.w.RecordSpawnEffort(sim.ConvID, args.Effort)
	s.w.RecordSpawnName(sim.ConvID, args.Name)
	s.w.RecordSpawnInitialPrompt(sim.ConvID, args.InitialPrompt)
	s.w.RecordSpawnTrustDir(sim.ConvID, args.TrustDir)
	s.w.RecordSpawnApproval(sim.ConvID, args.Approval)
	s.w.RecordSpawnSandbox(sim.ConvID, args.Sandbox)
	s.w.RecordSpawnSandboxImplementation(sim.ConvID, args.SandboxImplementation)
	s.w.RecordSpawnSandboxPolicy(sim.ConvID, args.EffectiveSandbox)
	// Production `session new` records a fresh launch's --name in the
	// conversation index (Gemini has no launch-name flag); mirror it.
	if args.Name != "" && args.ConvID == "" {
		if err := db.SetConvIndexCustomTitle(sim.ConvID, args.Name, geminiHarnessName); err != nil {
			return err
		}
	}
	if err := saveSessionWithResumeProvenance(&db.SessionRow{
		ID:                       label,
		TmuxSession:              label,
		ConvID:                   sim.ConvID,
		Cwd:                      sim.Cwd,
		Status:                   "running",
		Harness:                  geminiHarnessName,
		SandboxImplementation:    args.SandboxImplementation,
		HarnessBuiltinMode:       launchHarnessBuiltinMode(geminiHarnessName, args.Sandbox, args.SandboxImplementation),
		HarnessBuiltinModeSource: args.SandboxChosenBy,
		EffectiveSandbox:         args.EffectiveSandbox,
		ApprovalPolicy:           args.Approval,
	}); err != nil {
		return err
	}
	if s.w.SpawnPaneDiesAtLaunch {
		sim.Shutdown()
		return nil
	}
	s.w.Tmux.Register(label, sim.Cwd, sim)
	s.w.Geminis.Set(label, sim)
	sim.SubmitLaunchPrompt()
	return nil
}
