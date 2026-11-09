package agentd

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/pkg/selfupdate"
)

// SetNodeUpdateServiceForTest replaces binary discovery, leaving update jobs,
// authorization and release reads on their production paths.
func SetNodeUpdateServiceForTest(service *selfupdate.Service) func() {
	nodeUpdates.Lock()
	previous := nodeUpdates.service
	nodeUpdates.service = service
	nodeUpdates.Unlock()
	return func() {
		nodeUpdates.Lock()
		nodeUpdates.service = previous
		nodeUpdates.Unlock()
	}
}

// SetSelfUpdateNowForTest fixes the self-update scheduler's clock.
func SetSelfUpdateNowForTest(now time.Time) func() {
	prev := selfUpdateNow
	selfUpdateNow = func() time.Time { return now }
	return func() { selfUpdateNow = prev }
}

// SelfUpdateScheduleTickForTest runs one scheduler tick at the given time.
func SelfUpdateScheduleTickForTest(now time.Time) { selfUpdateScheduleTick(now) }

// FinishRestartedUpdateForTest runs the new daemon's post-restart step
// (post-update setup, automatic-run notification) for the current service.
func FinishRestartedUpdateForTest(svc *selfupdate.Service) {
	finishedRestarts.Delete(svc.Status().Job.ID)
	finishRestartedUpdate(context.Background(), svc)
}

// SelfUpdateJobFinishedForTest is the service Finished hook production wires
// in localUpdateService, for services built by tests.
func SelfUpdateJobFinishedForTest(job selfupdate.Job) { selfUpdateJobFinished(job) }
