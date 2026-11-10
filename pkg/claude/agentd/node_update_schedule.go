package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

// Automatic self-update runs: the updates.self schedule and update_on_start.
// Both are opt-in: they do nothing until the operator has chosen a method and
// enabled them. Runs go through the ordinary update job (durable ID, audit,
// restart verification); this file only decides when to start one and tells
// the operator how it went.

const (
	actorScheduler = "scheduler"
	actorStartup   = "startup"
	scheduleTick   = 30 * time.Second
)

// selfUpdateScheduledRun is the last automatic run, shown in settings.
type selfUpdateScheduledRun struct {
	At      time.Time `json:"at"`
	Trigger string    `json:"trigger"` // schedule | startup
	// Outcome: running | restarting | applied | up_to_date | update_available
	// | deferred | busy | failed
	Outcome string `json:"outcome"`
	JobID   string `json:"job_id,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type selfUpdateScheduleState struct {
	// LastSlot is the schedule window occurrence already run.
	LastSlot string                  `json:"last_slot,omitempty"`
	LastRun  *selfUpdateScheduledRun `json:"last_run,omitempty"`
}

var selfUpdateSchedule struct {
	sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var selfUpdateStateMu sync.Mutex

// selfUpdateNow is the scheduler's clock; tests replace it.
var selfUpdateNow = time.Now

func selfUpdateStatePath() string {
	return filepath.Join(common.TclaudeDataDir(), "updates", "schedule.json")
}

func loadSelfUpdateScheduleState() selfUpdateScheduleState {
	selfUpdateStateMu.Lock()
	defer selfUpdateStateMu.Unlock()
	return readSelfUpdateScheduleState()
}

func readSelfUpdateScheduleState() selfUpdateScheduleState {
	var s selfUpdateScheduleState
	if raw, err := os.ReadFile(selfUpdateStatePath()); err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// updateSelfUpdateScheduleState is a serialized read-modify-write.
func updateSelfUpdateScheduleState(mutate func(*selfUpdateScheduleState)) {
	selfUpdateStateMu.Lock()
	defer selfUpdateStateMu.Unlock()
	s := readSelfUpdateScheduleState()
	mutate(&s)
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	path := selfUpdateStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Warn("self-update: cannot write schedule state", "error", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		err = os.Rename(tmp, path)
		if err != nil {
			slog.Warn("self-update: cannot write schedule state", "error", err)
		}
	}
}

func notifySelfUpdate(subject, body string) {
	if _, err := db.InsertHumanMessage(&db.HumanMessage{FromTitle: "tclaude update", Subject: subject, Body: body}); err != nil {
		slog.Warn("self-update: failed to post notification", "error", err)
	}
}

func automaticActor(actor string) bool { return actor == actorScheduler || actor == actorStartup }

func triggerOf(actor string) string {
	if actor == actorStartup {
		return "startup"
	}
	return "schedule"
}

// startSelfUpdateScheduler runs once at daemon start, after the restart hook
// is configured: it finishes a just-restarted update (post-update setup,
// notification), runs update_on_start, and starts the schedule loop.
func startSelfUpdateScheduler() {
	selfUpdateSchedule.Lock()
	defer selfUpdateSchedule.Unlock()
	if selfUpdateSchedule.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	selfUpdateSchedule.cancel = cancel
	selfUpdateSchedule.wg.Add(1)
	go func() {
		defer selfUpdateSchedule.wg.Done()
		runSelfUpdateStartup(ctx)
		t := time.NewTicker(scheduleTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				selfUpdateScheduleTick(selfUpdateNow())
			}
		}
	}()
}

func stopSelfUpdateScheduler() {
	selfUpdateSchedule.Lock()
	cancel := selfUpdateSchedule.cancel
	selfUpdateSchedule.cancel = nil
	selfUpdateSchedule.Unlock()
	if cancel != nil {
		cancel()
		selfUpdateSchedule.wg.Wait()
	}
}

// runSelfUpdateStartup touches the updater only when an update has run on
// this node before or the operator enabled an automatic run; a node that
// never used the updater does no binary discovery at start.
func runSelfUpdateStartup(ctx context.Context) {
	s := loadSelfUpdateSettings()
	_, statErr := os.Stat(filepath.Join(common.TclaudeDataDir(), "updates", "status.json"))
	if statErr != nil && !s.UpdateOnStart {
		return
	}
	svc, err := localUpdateService()
	if err != nil {
		if s.UpdateOnStart && s.Method != "" {
			slog.Warn("self-update: updater unavailable at start", "error", err)
		}
		return
	}
	finishRestartedUpdate(ctx, svc)
	if !s.UpdateOnStart || s.Method == "" || ctx.Err() != nil {
		return
	}
	if j := svc.Status().Job; j != nil && j.FinishedAt != nil && selfUpdateNow().Sub(*j.FinishedAt) < 2*time.Minute {
		return // this start IS the restart after an update
	}
	startAutomaticUpdate(actorStartup, s)
}

func loadSelfUpdateSettings() config.SelfUpdateConfig {
	cfg, _ := config.Load()
	return cfg.SelfUpdate()
}

// selfUpdateScheduleTick starts the scheduled run once per window occurrence.
func selfUpdateScheduleTick(now time.Time) {
	s := loadSelfUpdateSettings()
	if s.Method == "" {
		return
	}
	slot, in := s.Schedule.Slot(now)
	if !in {
		return
	}
	state := loadSelfUpdateScheduleState()
	if state.LastSlot == slot {
		return
	}
	if s.Schedule.DeferWhileAgentsWorking && snapshotAgentTrayState().busy > 0 {
		if state.LastRun == nil || state.LastRun.Outcome != "deferred" {
			updateSelfUpdateScheduleState(func(st *selfUpdateScheduleState) {
				st.LastRun = &selfUpdateScheduledRun{At: now.UTC(), Trigger: "schedule", Outcome: "deferred", Detail: "agents are working; retrying within the window"}
			})
		}
		return
	}
	if run := startAutomaticUpdate(actorScheduler, s); run.Outcome != "busy" {
		updateSelfUpdateScheduleState(func(st *selfUpdateScheduleState) { st.LastSlot = slot })
	}
}

// startAutomaticUpdate starts the chosen method's job for an automatic run.
// manual only checks (and notifies when an update is available).
func startAutomaticUpdate(actor string, s config.SelfUpdateConfig) selfUpdateScheduledRun {
	run := selfUpdateScheduledRun{At: selfUpdateNow().UTC(), Trigger: triggerOf(actor)}
	defer func() {
		r := run
		updateSelfUpdateScheduleState(func(st *selfUpdateScheduleState) {
			// Finished may already have recorded this job's outcome.
			if r.Outcome != "running" || st.LastRun == nil || st.LastRun.JobID != r.JobID {
				st.LastRun = &r
			}
		})
	}()
	svc, err := localUpdateService()
	if err != nil {
		run.Outcome, run.Detail = "failed", err.Error()
		notifySelfUpdate("Automatic tclaude update failed", "The updater is unavailable: "+err.Error())
		return run
	}
	req := selfupdate.Request{Action: "apply"}
	if s.Method == selfupdate.MethodManual {
		req.Action = "check"
	}
	run.Outcome = "running"
	opts := selfUpdateApplyOptions(s)
	if req.Action == "apply" {
		opts.Launch = selfUpdateLaunchPlan()
	}
	// Set before the job starts: Finished can run before StartWith returns.
	job, err := svc.StartWith(req, actor, func() bool { return true }, opts)
	switch {
	case errors.Is(err, selfupdate.ErrBusy):
		run.Outcome, run.Detail = "busy", "another update job is running; retrying"
	case err != nil:
		run.Outcome, run.Detail = "failed", err.Error()
		notifySelfUpdate("Automatic tclaude update did not start", err.Error())
	default:
		run.JobID = job.ID
	}
	return run
}

// selfUpdateJobFinished is the service's Finished hook for automatic runs.
func selfUpdateJobFinished(job selfupdate.Job) {
	if !automaticActor(job.Actor) {
		return
	}
	run := selfUpdateScheduledRun{At: selfUpdateNow().UTC(), Trigger: triggerOf(job.Actor), JobID: job.ID}
	switch {
	case job.State == "failed":
		run.Outcome, run.Detail = "failed", job.Error
		notifySelfUpdate("Automatic tclaude update failed", fmt.Sprintf("%s %s failed: %s", job.Action, job.Version, job.Error))
	case job.State == "restarting":
		run.Outcome = "restarting"
	case job.Action == "check" && job.UpdateAvailable != nil && *job.UpdateAvailable:
		run.Outcome = "update_available"
		notifySelfUpdate("tclaude "+job.Version+" is available", fmt.Sprintf("This node runs %s. The update method is manual: install %s yourself, or choose a method with tclaude update config set method <m>.", job.CurrentVersion, job.Version))
	default:
		run.Outcome = "up_to_date"
	}
	updateSelfUpdateScheduleState(func(st *selfUpdateScheduleState) { st.LastRun = &run })
}

var finishedRestarts sync.Map

// finishRestartedUpdate runs in the new daemon after an update restart: the
// pending post-update setup with the NEW tclaude binary, then the result
// notification for automatic runs.
func finishRestartedUpdate(ctx context.Context, svc *selfupdate.Service) {
	// First, while nothing else has changed: did the restart keep the
	// daemon's flags and environment?
	compareRestartedLaunch(svc)
	job := svc.Status().Job
	if job == nil || job.Action != "apply" || job.State != "succeeded" && job.State != "failed" {
		return
	}
	if _, done := finishedRestarts.LoadOrStore(job.ID, true); done {
		return
	}
	setupNote := ""
	if pending := svc.PendingPostSetup(); pending != nil {
		tclaude := ""
		for _, b := range svc.Status().Binaries {
			if b.Name == "tclaude" {
				tclaude = b.Path
			}
		}
		if tclaude == "" {
			setupNote = "Post-update setup skipped: no tclaude binary was found."
		} else if done, err := svc.RunPostSetup(ctx, tclaude); err != nil {
			setupNote = "Post-update setup could not run: " + err.Error()
		} else {
			*job = done
			ps := done.PostSetup
			setupNote = fmt.Sprintf("Post-update setup (tclaude %v) %s with exit code %d.", ps.Args, ps.State, *ps.ExitCode)
			if ps.State == "failed" {
				setupNote += "\n" + ps.Output
				if !automaticActor(job.Actor) {
					notifySelfUpdate("Post-update setup failed", setupNote)
				}
			}
		}
	}
	if !automaticActor(job.Actor) {
		return
	}
	state := loadSelfUpdateScheduleState()
	if state.LastRun == nil || state.LastRun.JobID != job.ID || state.LastRun.Outcome != "restarting" {
		return
	}
	run := *state.LastRun
	run.At = selfUpdateNow().UTC()
	if job.State == "succeeded" {
		run.Outcome = "applied"
		body := fmt.Sprintf("The %s update from %s to %s completed and agentd restarted.", triggerOf(job.Actor), job.CurrentVersion, job.Version)
		for _, note := range []string{job.Launch.Summary(), setupNote} {
			if note != "" {
				body += " " + note
			}
		}
		notifySelfUpdate("tclaude updated to "+job.Version, body)
	} else {
		run.Outcome, run.Detail = "failed", job.Error
		notifySelfUpdate("Automatic tclaude update failed", job.Error)
	}
	updateSelfUpdateScheduleState(func(st *selfUpdateScheduleState) { st.LastRun = &run })
}
