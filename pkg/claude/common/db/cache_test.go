package db

import (
	"encoding/json"
	"testing"
	"time"
)

func TestUsageCacheCRUD(t *testing.T) {
	setupTestDB(t)

	// Initially empty
	row, err := LoadUsageCache()
	if err != nil {
		t.Fatalf("LoadUsageCache: %v", err)
	}
	if row != nil {
		t.Fatal("expected nil for empty cache")
	}

	// Save
	data := json.RawMessage(`{"five_hour":{"pct":42}}`)
	now := time.Now().Truncate(time.Millisecond)
	if err := SaveUsageCache(data, now, now); err != nil {
		t.Fatalf("SaveUsageCache: %v", err)
	}

	// Load back
	row, err = LoadUsageCache()
	if err != nil {
		t.Fatalf("LoadUsageCache: %v", err)
	}
	if row == nil {
		t.Fatal("expected non-nil cache row")
	}
	if string(row.Data) != `{"five_hour":{"pct":42}}` {
		t.Errorf("data = %s, want %s", row.Data, `{"five_hour":{"pct":42}}`)
	}

	// Update
	data2 := json.RawMessage(`{"five_hour":{"pct":77}}`)
	if err := SaveUsageCache(data2, now, now); err != nil {
		t.Fatalf("SaveUsageCache update: %v", err)
	}
	row, _ = LoadUsageCache()
	if string(row.Data) != `{"five_hour":{"pct":77}}` {
		t.Errorf("after update, data = %s", row.Data)
	}

	// Delete
	if err := DeleteUsageCache(); err != nil {
		t.Fatalf("DeleteUsageCache: %v", err)
	}
	row, _ = LoadUsageCache()
	if row != nil {
		t.Fatal("expected nil after delete")
	}
}

func TestTryClaimUsageFetch_FirstClaim(t *testing.T) {
	setupTestDB(t)

	ttl := 5 * time.Minute

	// First claim on empty table should succeed
	claimed, err := TryClaimUsageFetch(ttl)
	if err != nil {
		t.Fatalf("TryClaimUsageFetch: %v", err)
	}
	if !claimed {
		t.Fatal("expected first claim to succeed")
	}

	// Second claim within TTL should fail
	claimed, err = TryClaimUsageFetch(ttl)
	if err != nil {
		t.Fatalf("TryClaimUsageFetch: %v", err)
	}
	if claimed {
		t.Fatal("expected second claim within TTL to fail")
	}
}

func TestTryClaimUsageFetch_ExpiredClaim(t *testing.T) {
	setupTestDB(t)

	ttl := 5 * time.Minute

	// Seed with an expired entry
	oldTime := time.Now().Add(-ttl - time.Minute)
	data := json.RawMessage(`{}`)
	if err := SaveUsageCache(data, oldTime, oldTime); err != nil {
		t.Fatalf("SaveUsageCache: %v", err)
	}

	// Claim should succeed since entry is expired
	claimed, err := TryClaimUsageFetch(ttl)
	if err != nil {
		t.Fatalf("TryClaimUsageFetch: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim on expired entry to succeed")
	}
}

func TestTryClaimUsageFetch_FreshEntry(t *testing.T) {
	setupTestDB(t)

	ttl := 5 * time.Minute

	// Seed with a fresh entry
	now := time.Now()
	data := json.RawMessage(`{"five_hour":{"pct":50}}`)
	if err := SaveUsageCache(data, now, now); err != nil {
		t.Fatalf("SaveUsageCache: %v", err)
	}

	// Claim should fail since entry is fresh
	claimed, err := TryClaimUsageFetch(ttl)
	if err != nil {
		t.Fatalf("TryClaimUsageFetch: %v", err)
	}
	if claimed {
		t.Fatal("expected claim on fresh entry to fail")
	}

	// Verify data wasn't corrupted
	row, err := LoadUsageCache()
	if err != nil {
		t.Fatalf("LoadUsageCache: %v", err)
	}
	if string(row.Data) != `{"five_hour":{"pct":50}}` {
		t.Errorf("data changed unexpectedly: %s", row.Data)
	}
}

func TestGitCacheCRUD(t *testing.T) {
	setupTestDB(t)

	// Initially empty
	row, err := LoadGitCache("abc123")
	if err != nil {
		t.Fatalf("LoadGitCache: %v", err)
	}
	if row != nil {
		t.Fatal("expected nil for empty cache")
	}

	// Save
	data := json.RawMessage(`{"branch":"main","repo_url":"https://github.com/test/repo"}`)
	now := time.Now().Truncate(time.Millisecond)
	if err := SaveGitCache("abc123", data, now); err != nil {
		t.Fatalf("SaveGitCache: %v", err)
	}

	// Load back
	row, err = LoadGitCache("abc123")
	if err != nil {
		t.Fatalf("LoadGitCache: %v", err)
	}
	if row == nil {
		t.Fatal("expected non-nil cache row")
	}
	if string(row.Data) != `{"branch":"main","repo_url":"https://github.com/test/repo"}` {
		t.Errorf("data = %s", row.Data)
	}

	// Different key returns nil
	row, _ = LoadGitCache("other-key")
	if row != nil {
		t.Fatal("expected nil for different key")
	}

	// Update same key
	data2 := json.RawMessage(`{"branch":"feature"}`)
	if err := SaveGitCache("abc123", data2, now); err != nil {
		t.Fatalf("SaveGitCache update: %v", err)
	}
	row, _ = LoadGitCache("abc123")
	if string(row.Data) != `{"branch":"feature"}` {
		t.Errorf("after update, data = %s", row.Data)
	}
}

func TestGitCache_MultipleRepos(t *testing.T) {
	setupTestDB(t)

	now := time.Now()
	if err := SaveGitCache("repo-a", json.RawMessage(`{"branch":"main"}`), now); err != nil {
		t.Fatalf("SaveGitCache repo-a: %v", err)
	}
	if err := SaveGitCache("repo-b", json.RawMessage(`{"branch":"develop"}`), now); err != nil {
		t.Fatalf("SaveGitCache repo-b: %v", err)
	}

	rowA, _ := LoadGitCache("repo-a")
	rowB, _ := LoadGitCache("repo-b")

	if string(rowA.Data) != `{"branch":"main"}` {
		t.Errorf("repo-a data = %s", rowA.Data)
	}
	if string(rowB.Data) != `{"branch":"develop"}` {
		t.Errorf("repo-b data = %s", rowB.Data)
	}
}

func TestSchemaV1ToV2Migration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	ResetForTest()

	// Open creates v2 schema from scratch
	d, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Verify the cache tables exist by inserting into them
	_, err = d.Exec(`INSERT INTO usage_cache (id, data, fetched_at, last_attempt_at) VALUES (1, '{}', '', '')`)
	if err != nil {
		t.Fatalf("insert into usage_cache: %v", err)
	}
	_, err = d.Exec(`INSERT INTO git_cache (repo_hash, data, fetched_at) VALUES ('test', '{}', '')`)
	if err != nil {
		t.Fatalf("insert into git_cache: %v", err)
	}

	// Verify schema version is 2
	var ver int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&ver); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if ver != 3 {
		t.Fatalf("expected version 3, got %d", ver)
	}
}
