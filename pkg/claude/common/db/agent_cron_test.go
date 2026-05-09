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
