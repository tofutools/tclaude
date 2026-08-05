package testharness

import (
	"fmt"
	"os"
	"path/filepath"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The simulated spawner's `--harness copilot` branches.
//
// They differ from the Codex ones in a way worth stating plainly: this branch
// renders the REAL production launch string (copilotBuildLaunchCommand →
// harness.copilotSpawner.BuildCommand) and starts the pane FROM it. Nothing
// else in this package does that, and for Copilot it is the point — the
// argv is where a detached launch succeeds or deadlocks, so a simulator that
// took structured arguments would agree with a broken spawner.
//
// A launch string the real CLI would reject therefore produces a pane that
// never exists: the session row lands (production writes it before creating
// the tmux session) and the spawn call fails, which is the shape an operator
// sees when a harness dies on a bad flag.

// copilotSpawnCwd mirrors the other branches' default: a launch with no cwd
// gets a home-rooted one that actually exists, so resume guards and cwd-scoped
// conversation listings have a real directory to resolve. It is rooted at the
// test HOME rather than at COPILOT_HOME, which is Copilot's state directory and
// never a workspace.
func (s *simSpawner) copilotSpawnCwd(cwd string) string {
	if cwd != "" {
		return cwd
	}
	cwd = filepath.Join(s.w.HomeDir, "copilot-sim-cwd")
	_ = os.MkdirAll(cwd, 0o755)
	return cwd
}

// spawnNewCopilot is SpawnNew's `--harness copilot` branch.
func (s *simSpawner) spawnNewCopilot(args clcommon.SpawnArgs) error {
	label := args.Label
	cwd := s.copilotSpawnCwd(args.Cwd)
	cmd, err := copilotBuildLaunchCommand(copilotLaunchArgs{
		Cwd:            cwd,
		SessionID:      args.SessionID,
		Name:           args.Name,
		Model:          args.Model,
		Effort:         args.Effort,
		InitialPrompt:  args.InitialPrompt,
		ApprovalPolicy: args.Approval,
	})
	if err != nil {
		return err
	}
	home := CopilotHomeFor(s.w.HomeDir)
	sim, err := NewCopilotSim(s.t, home, cwd, cmd)
	if err != nil {
		return fmt.Errorf("copilot launch would not start: %w", err)
	}
	sim.SetSessionID(label)
	s.w.RecordCopilotLaunchCommand(sim.ConvID, cmd)
	s.seedCopilotDirTrust(args, cwd)
	if err := sim.Start(); err != nil {
		return err
	}
	s.recordCopilotSpawnObservability(sim.ConvID, args)
	if err := saveSessionWithResumeProvenance(&db.SessionRow{
		ID:                       label,
		TmuxSession:              label,
		ConvID:                   sim.ConvID,
		Cwd:                      sim.Cwd,
		Status:                   "running",
		Harness:                  copilotHarnessName,
		HarnessBuiltinMode:       launchHarnessBuiltinMode(copilotHarnessName, args.Sandbox, args.SandboxImplementation),
		SandboxImplementation:    args.SandboxImplementation,
		HarnessBuiltinModeSource: args.SandboxChosenBy,
		EffectiveSandbox:         args.EffectiveSandbox,
		ApprovalPolicy:           args.Approval,
		ApprovalAutoReview:       args.AutoReview,
		AskUserQuestionTimeout:   args.AskUserQuestionTimeout,
	}); err != nil {
		return err
	}
	if s.w.SpawnPaneDiesAtLaunch {
		sim.Shutdown()
		return nil
	}
	s.w.Tmux.Register(label, sim.Cwd, sim)
	s.w.Copilots.Set(label, sim)
	// Last, mirroring production's ordering: the row is written before the tmux
	// session is created, so the `-i` first turn's hooks land on a row that
	// already exists.
	sim.SubmitLaunchPrompt()
	return nil
}

// spawnResumeCopilot is SpawnResume's `--harness copilot` branch.
//
// A Copilot resume reopens the SAME session-state directory and APPENDS a
// session.resume record to the existing events.jsonl, so re-attaching the
// existing sim (rather than minting a new one) is faithful. Where no sim
// exists — a conversation seeded on disk by the test — a fresh one is built
// against the same conv-id, which reproduces a daemon restart relaunching a
// pane it did not create.
func (s *simSpawner) spawnResumeCopilot(args clcommon.SpawnArgs) error {
	convID := args.ConvID
	cwd := s.copilotSpawnCwd(args.Cwd)
	cmd, err := copilotBuildLaunchCommand(copilotLaunchArgs{
		Cwd:            cwd,
		ResumeID:       convID,
		Model:          args.Model,
		Effort:         args.Effort,
		InitialPrompt:  args.InitialPrompt,
		ApprovalPolicy: args.Approval,
	})
	if err != nil {
		return err
	}
	home := CopilotHomeFor(s.w.HomeDir)
	sim := s.w.Copilots.GetByConvID(convID)
	if sim == nil {
		fresh, buildErr := NewCopilotSim(s.t, home, cwd, cmd)
		if buildErr != nil {
			return fmt.Errorf("copilot relaunch would not start: %w", buildErr)
		}
		sim = fresh
	} else {
		// An existing pane is relaunched under the resume argv: re-parse it so
		// a posture the relaunch failed to preserve is visible to the gates.
		relaunch, parseErr := ParseCopilotLaunch(cmd)
		if parseErr != nil {
			return fmt.Errorf("copilot relaunch would not start: %w", parseErr)
		}
		sim.mu.Lock()
		sim.launch = relaunch
		sim.alive = false
		sim.mu.Unlock()
	}
	s.w.RecordCopilotLaunchCommand(convID, cmd)
	s.seedCopilotDirTrust(args, sim.Cwd)
	if err := sim.Start(); err != nil {
		return err
	}
	s.recordCopilotSpawnObservability(convID, args)
	label := generateResumeLabel()
	sim.SetSessionID(label)
	if err := saveSessionWithResumeProvenance(&db.SessionRow{
		ID:                       label,
		TmuxSession:              label,
		ConvID:                   convID,
		Cwd:                      sim.Cwd,
		Status:                   "running",
		Harness:                  copilotHarnessName,
		HarnessBuiltinMode:       launchHarnessBuiltinMode(copilotHarnessName, args.Sandbox, args.SandboxImplementation),
		SandboxImplementation:    args.SandboxImplementation,
		HarnessBuiltinModeSource: args.SandboxChosenBy,
		EffectiveSandbox:         args.EffectiveSandbox,
		ApprovalPolicy:           args.Approval,
		ApprovalAutoReview:       args.AutoReview,
		AskUserQuestionTimeout:   args.AskUserQuestionTimeout,
	}); err != nil {
		return err
	}
	if s.w.SpawnPaneDiesAtLaunch {
		sim.Shutdown()
		return nil
	}
	s.w.Tmux.Register(label, sim.Cwd, sim)
	s.w.Copilots.Set(label, sim)
	sim.SubmitLaunchPrompt()
	return nil
}

// recordCopilotSpawnObservability mirrors the other branches' capture of what
// the spawn path threaded, keyed by conv-id, so the existing cross-harness
// flow assertions work for Copilot without a Copilot-specific accessor.
func (s *simSpawner) recordCopilotSpawnObservability(convID string, args clcommon.SpawnArgs) {
	s.w.RecordSpawnEffort(convID, args.Effort)
	s.w.RecordSpawnModel(convID, args.Model)
	s.w.RecordSpawnSandbox(convID, args.Sandbox)
	s.w.RecordSpawnSandboxPolicy(convID, args.EffectiveSandbox)
	s.w.RecordSpawnAskTimeout(convID, args.AskUserQuestionTimeout)
	s.w.RecordSpawnApproval(convID, args.Approval)
	s.w.RecordSpawnAutoReview(convID, args.AutoReview)
	s.w.RecordSpawnTrustDir(convID, args.TrustDir)
	s.w.RecordSpawnRemoteControl(convID, args.RemoteControl)
	s.w.RecordSpawnSandboxImplementation(convID, args.SandboxImplementation)
	s.w.RecordSpawnCwdWriteProof(convID, args.CwdWriteProof)
	s.w.RecordSpawnDirWriteProof(convID, args.DirWriteProof)
	s.w.RecordSpawnGitWorktreeWriteDirs(convID, args.GitWorktreeWriteDirs)
	s.w.RecordSpawnName(convID, args.Name)
	s.w.RecordSpawnInitialPrompt(convID, args.InitialPrompt)
}

// seedCopilotDirTrust reproduces what production's `tclaude session new` does
// immediately before it starts the pane: when the launch opted into
// pre-trusting its cwd, seed the harness's trust store so the agent does not
// freeze on the folder-trust modal.
//
// It calls the PRODUCTION editor rather than writing the file, for the same
// reason the pane boots from the production spawner's argv: this simulator is
// what will validate tclaude's seeding, so an imitation of the write would let
// a broken editor pass. Both ways a spawn can reach it are covered without a
// second code path — an explicit `--trust-dir` / profile / dashboard opt-in,
// and the daemon's own defaultSiblingWorktreeTrust auto-enable — because both
// arrive here as the same resolved SpawnArgs.TrustDir.
//
// Best-effort, exactly as production is: a seeding failure warns and the launch
// continues, leaving the operator the dashboard focus button. Here that shows
// up as a pane that parks, which is the honest outcome.
func (s *simSpawner) seedCopilotDirTrust(args clcommon.SpawnArgs, cwd string) {
	if !args.TrustDir {
		return
	}
	TrustCopilotFolder(s.t, CopilotHomeFor(s.w.HomeDir), cwd)
}
