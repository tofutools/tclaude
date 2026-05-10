package db

import (
	"testing"
)

func TestRecordConvSuccession_Roundtrip(t *testing.T) {
	setupTestDB(t)
	if err := RecordConvSuccession("aaaa", "bbbb", "reincarnate"); err != nil {
		t.Fatalf("RecordConvSuccession: %v", err)
	}
	got, err := GetConvSuccessor("aaaa")
	if err != nil {
		t.Fatalf("GetConvSuccessor: %v", err)
	}
	if got != "bbbb" {
		t.Errorf("successor = %q, want %q", got, "bbbb")
	}
}

func TestRecordConvSuccession_Idempotent(t *testing.T) {
	setupTestDB(t)
	// First write — establishes the chain edge.
	if err := RecordConvSuccession("aaaa", "bbbb", "reincarnate"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// Re-write the same edge — should overwrite, not error. (In
	// practice reincarnate never re-records the same edge, but the
	// idempotency keeps the contract robust against retries.)
	if err := RecordConvSuccession("aaaa", "bbbb", "reincarnate"); err != nil {
		t.Fatalf("re-write same edge: %v", err)
	}
	// Update to a different successor — also should overwrite.
	if err := RecordConvSuccession("aaaa", "cccc", "clone-replace"); err != nil {
		t.Fatalf("change edge: %v", err)
	}
	got, _ := GetConvSuccessor("aaaa")
	if got != "cccc" {
		t.Errorf("after edge update, successor = %q, want %q", got, "cccc")
	}
}

func TestRecordConvSuccession_Rejects(t *testing.T) {
	setupTestDB(t)
	if err := RecordConvSuccession("", "bbbb", "x"); err == nil {
		t.Error("expected error for empty oldConv")
	}
	if err := RecordConvSuccession("aaaa", "", "x"); err == nil {
		t.Error("expected error for empty newConv")
	}
	if err := RecordConvSuccession("aaaa", "aaaa", "x"); err == nil {
		t.Error("expected error for old == new")
	}
}

func TestResolveLatestConv_WalksChain(t *testing.T) {
	setupTestDB(t)
	// A → B → C → D
	for _, edge := range [][2]string{
		{"aaaa", "bbbb"},
		{"bbbb", "cccc"},
		{"cccc", "dddd"},
	} {
		if err := RecordConvSuccession(edge[0], edge[1], "reincarnate"); err != nil {
			t.Fatalf("RecordConvSuccession: %v", err)
		}
	}
	cases := map[string]string{
		"aaaa": "dddd", // four hops back → live
		"bbbb": "dddd",
		"cccc": "dddd",
		"dddd": "dddd", // already live
		"eeee": "eeee", // no chain → returns input
	}
	for in, want := range cases {
		got := ResolveLatestConv(in)
		if got != want {
			t.Errorf("ResolveLatestConv(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMigrateCronJobConvRef(t *testing.T) {
	setupTestDB(t)

	// Two jobs: one owned by `oldconv`, one targeted at `oldconv`.
	owner, err := InsertAgentCronJob(&AgentCronJob{
		Name:            "owned-by-old",
		OwnerConv:       "oldconv",
		TargetConv:      "someother",
		IntervalSeconds: 60,
		Body:            "ping",
		Enabled:         true,
	})
	if err != nil {
		t.Fatalf("insert owner job: %v", err)
	}
	target, err := InsertAgentCronJob(&AgentCronJob{
		Name:            "targets-old",
		OwnerConv:       "manager",
		TargetConv:      "oldconv",
		IntervalSeconds: 120,
		Body:            "status?",
		Enabled:         true,
	})
	if err != nil {
		t.Fatalf("insert target job: %v", err)
	}
	// Sanity: a third job that doesn't reference oldconv at all.
	bystander, err := InsertAgentCronJob(&AgentCronJob{
		Name:            "untouched",
		OwnerConv:       "elsewhere",
		TargetConv:      "elsewhere2",
		IntervalSeconds: 30,
		Body:            "ok",
		Enabled:         true,
	})
	if err != nil {
		t.Fatalf("insert bystander job: %v", err)
	}

	n, err := MigrateCronJobConvRef("oldconv", "newconv")
	if err != nil {
		t.Fatalf("MigrateCronJobConvRef: %v", err)
	}
	if n != 2 {
		t.Errorf("rows affected = %d, want 2", n)
	}

	got1, _ := GetAgentCronJob(owner)
	if got1.OwnerConv != "newconv" {
		t.Errorf("owner job owner_conv = %q, want %q", got1.OwnerConv, "newconv")
	}
	if got1.TargetConv != "someother" {
		t.Errorf("owner job target_conv mutated unexpectedly: %q", got1.TargetConv)
	}
	got2, _ := GetAgentCronJob(target)
	if got2.TargetConv != "newconv" {
		t.Errorf("target job target_conv = %q, want %q", got2.TargetConv, "newconv")
	}
	got3, _ := GetAgentCronJob(bystander)
	if got3.OwnerConv != "elsewhere" || got3.TargetConv != "elsewhere2" {
		t.Errorf("bystander mutated: owner=%q target=%q", got3.OwnerConv, got3.TargetConv)
	}
}

func TestListAgentConvSuccessions_OrderedByRecency(t *testing.T) {
	setupTestDB(t)
	if err := RecordConvSuccession("a1", "a2", "reincarnate"); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if err := RecordConvSuccession("b1", "b2", "reincarnate"); err != nil {
		t.Fatalf("second record: %v", err)
	}
	rows, err := ListAgentConvSuccessions()
	if err != nil {
		t.Fatalf("ListAgentConvSuccessions: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	// Most recent first. RFC3339 succeeded_at has 1-second precision so
	// rapid back-to-back writes can collide; the rowid-DESC tiebreaker
	// guarantees deterministic ordering regardless of clock granularity.
	if rows[0].OldConvID != "b1" {
		t.Errorf("rows[0].OldConvID = %q, want %q", rows[0].OldConvID, "b1")
	}
}
