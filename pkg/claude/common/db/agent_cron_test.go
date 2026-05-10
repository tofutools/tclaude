package db

import (
	"testing"
	"time"
)

func TestAgentCronJob_InsertGetList(t *testing.T) {
	setupTestDB(t)

	id, err := InsertAgentCronJob(&AgentCronJob{
		Name:            "po-pings",
		OwnerConv:       "po-conv",
		TargetConv:      "worker-conv",
		GroupID:         42,
		IntervalSeconds: 600,
		Subject:         "status check",
		Body:            "What's the latest?",
		Enabled:         true,
	})
	if err != nil {
		t.Fatalf("InsertAgentCronJob: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	got, err := GetAgentCronJob(id)
	if err != nil {
		t.Fatalf("GetAgentCronJob: %v", err)
	}
	if got == nil {
		t.Fatal("got nil row")
	}
	if got.Name != "po-pings" || got.OwnerConv != "po-conv" || got.TargetConv != "worker-conv" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.IntervalSeconds != 600 {
		t.Errorf("interval: got %d, want 600", got.IntervalSeconds)
	}
	if !got.Enabled {
		t.Error("expected enabled=true")
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at should be stamped on insert")
	}
	if !got.LastRunAt.IsZero() {
		t.Error("last_run_at should be zero before any fire")
	}

	all, err := ListAgentCronJobs()
	if err != nil {
		t.Fatalf("ListAgentCronJobs: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 job, got %d", len(all))
	}
}

func TestAgentCronJob_DueLogic(t *testing.T) {
	setupTestDB(t)

	// j1: never run, always due.
	j1, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})
	// j2: ran 30s ago with a 60s interval — NOT due.
	j2, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})
	// j3: ran 90s ago with a 60s interval — due.
	j3, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})
	// j4: disabled, even though it'd be due — must NOT show up.
	j4, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: false,
	})

	now := time.Now()
	if err := UpdateAgentCronJobLastRun(j2, now.Add(-30*time.Second), "ok"); err != nil {
		t.Fatalf("stamp j2: %v", err)
	}
	if err := UpdateAgentCronJobLastRun(j3, now.Add(-90*time.Second), "ok"); err != nil {
		t.Fatalf("stamp j3: %v", err)
	}
	if err := UpdateAgentCronJobLastRun(j4, now.Add(-90*time.Second), "ok"); err != nil {
		t.Fatalf("stamp j4: %v", err)
	}

	due, err := ListDueAgentCronJobs(now)
	if err != nil {
		t.Fatalf("ListDueAgentCronJobs: %v", err)
	}
	dueIDs := map[int64]bool{}
	for _, j := range due {
		dueIDs[j.ID] = true
	}
	if !dueIDs[j1] {
		t.Errorf("j1 (never run) should be due")
	}
	if dueIDs[j2] {
		t.Errorf("j2 (30s ago, 60s interval) should NOT be due")
	}
	if !dueIDs[j3] {
		t.Errorf("j3 (90s ago, 60s interval) should be due")
	}
	if dueIDs[j4] {
		t.Errorf("j4 (disabled) should never be due")
	}
}

func TestAgentCronRun_InsertListCascade(t *testing.T) {
	setupTestDB(t)

	id, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})

	// Three runs at distinct timestamps.
	t0 := time.Now()
	for i, dt := range []time.Duration{-2 * time.Hour, -1 * time.Hour, 0} {
		_, err := InsertAgentCronRun(&AgentCronRun{
			JobID:   id,
			FiredAt: t0.Add(dt),
			Status:  "ok",
		})
		if err != nil {
			t.Fatalf("insert run %d: %v", i, err)
		}
	}

	// Newest first.
	runs, err := ListAgentCronRunsForJob(id, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(runs))
	}
	if !runs[0].FiredAt.After(runs[1].FiredAt) || !runs[1].FiredAt.After(runs[2].FiredAt) {
		t.Errorf("runs not sorted newest-first: %+v", runs)
	}

	// Limit truncates from the head (newest).
	limited, _ := ListAgentCronRunsForJob(id, 2)
	if len(limited) != 2 {
		t.Errorf("limit=2 → got %d", len(limited))
	}

	// Cascade on job delete.
	if err := DeleteAgentCronJob(id); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	after, _ := ListAgentCronRunsForJob(id, 0)
	if len(after) != 0 {
		t.Errorf("expected runs cascaded with job delete; got %d", len(after))
	}
}

func TestAgentCronJob_UpdateFields_Partial(t *testing.T) {
	setupTestDB(t)

	id, _ := InsertAgentCronJob(&AgentCronJob{
		Name:            "before",
		OwnerConv:       "owner",
		TargetConv:      "target",
		GroupID:         7,
		IntervalSeconds: 300,
		Subject:         "subj-before",
		Body:            "body-before",
		Enabled:         true,
	})
	// Stamp a non-zero last_run_at so we can prove UpdateAgentCronJobFields
	// leaves it alone.
	prevRun := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	if err := UpdateAgentCronJobLastRun(id, prevRun, "ok"); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	// Touch only name + enabled. All other fields should be unchanged.
	newName := "after"
	enabled := false
	n, err := UpdateAgentCronJobFields(id, UpdateCronPatch{Name: &newName, Enabled: &enabled})
	if err != nil {
		t.Fatalf("UpdateAgentCronJobFields: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows affected = %d, want 1", n)
	}
	got, _ := GetAgentCronJob(id)
	if got.Name != "after" {
		t.Errorf("name: got %q, want %q", got.Name, "after")
	}
	if got.Enabled {
		t.Errorf("expected enabled=false after patch")
	}
	// Untouched.
	if got.OwnerConv != "owner" || got.TargetConv != "target" || got.GroupID != 7 ||
		got.IntervalSeconds != 300 || got.Subject != "subj-before" || got.Body != "body-before" {
		t.Errorf("untouched fields changed: %+v", got)
	}
	// last_run_at preserved.
	if !got.LastRunAt.Equal(prevRun) {
		t.Errorf("last_run_at must not be touched; got %v, want %v", got.LastRunAt, prevRun)
	}
	if got.LastRunStatus != "ok" {
		t.Errorf("last_run_status must not be touched; got %q, want %q", got.LastRunStatus, "ok")
	}
}

func TestAgentCronJob_UpdateFields_IntervalLeavesLastRunAlone(t *testing.T) {
	setupTestDB(t)

	id, _ := InsertAgentCronJob(&AgentCronJob{
		Name:            "n",
		OwnerConv:       "a",
		TargetConv:      "b",
		IntervalSeconds: 60,
		Body:            "x",
		Enabled:         true,
	})
	stamped := time.Now().Add(-90 * time.Second).UTC().Truncate(time.Second)
	if err := UpdateAgentCronJobLastRun(id, stamped, "ok"); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	newInterval := int64(3600)
	if _, err := UpdateAgentCronJobFields(id, UpdateCronPatch{IntervalSeconds: &newInterval}); err != nil {
		t.Fatalf("UpdateAgentCronJobFields: %v", err)
	}
	got, _ := GetAgentCronJob(id)
	if got.IntervalSeconds != 3600 {
		t.Errorf("interval: got %d, want 3600", got.IntervalSeconds)
	}
	if !got.LastRunAt.Equal(stamped) {
		t.Errorf("last_run_at changed after interval patch: got %v, want %v", got.LastRunAt, stamped)
	}
}

func TestAgentCronJob_UpdateFields_EmptyPatchNoop(t *testing.T) {
	setupTestDB(t)
	id, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})
	n, err := UpdateAgentCronJobFields(id, UpdateCronPatch{})
	if err != nil {
		t.Fatalf("UpdateAgentCronJobFields: %v", err)
	}
	if n != 0 {
		t.Errorf("empty patch should affect 0 rows; got %d", n)
	}
}

func TestAgentCronJob_UpdateFields_NotFound(t *testing.T) {
	setupTestDB(t)
	newName := "x"
	n, err := UpdateAgentCronJobFields(9999, UpdateCronPatch{Name: &newName})
	if err != nil {
		t.Fatalf("UpdateAgentCronJobFields: %v", err)
	}
	if n != 0 {
		t.Errorf("missing id should affect 0 rows; got %d", n)
	}
}

func TestAgentCronJob_DeleteAndEnable(t *testing.T) {
	setupTestDB(t)

	id, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})

	if err := SetAgentCronJobEnabled(id, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _ := GetAgentCronJob(id)
	if got.Enabled {
		t.Errorf("expected enabled=false after disable")
	}

	if err := DeleteAgentCronJob(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ = GetAgentCronJob(id)
	if got != nil {
		t.Errorf("expected nil after delete; got %+v", got)
	}
	// Idempotent on re-delete.
	if err := DeleteAgentCronJob(id); err != nil {
		t.Errorf("re-delete should be no-op; got %v", err)
	}
}
