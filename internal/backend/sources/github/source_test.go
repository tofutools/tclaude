package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var testTime = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

func request() ports.AutomationFactCollectRequest {
	return ports.AutomationFactCollectRequest{Resource: model.AutomationFactResource{Kind: model.FactResourceRepositoryPullReq, Repository: "owner/repo", PullRequest: 7}, Limit: 2, Now: testTime}
}
func respondJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func pullJSON(sha string) map[string]any {
	return map[string]any{"number": 7, "state": "open", "updated_at": testTime.Add(-time.Minute), "head": map[string]string{"sha": sha}, "base": map[string]any{"repo": map[string]string{"full_name": "owner/repo"}}}
}
func setup(t *testing.T, override func(http.ResponseWriter, *http.Request) bool) *Source {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected method/credential")
		}
		if override != nil && override(w, r) {
			return
		}
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/7":
			respondJSON(w, pullJSON(testSHA))
		case "/repos/owner/repo/commits/" + testSHA + "/check-runs":
			respondJSON(w, map[string]any{"total_count": 1, "check_runs": []map[string]any{{"id": 1, "status": "completed", "conclusion": "success", "completed_at": testTime.Add(-time.Second)}}})
		case "/repos/owner/repo/commits/" + testSHA + "/status":
			respondJSON(w, map[string]any{"sha": testSHA, "total_count": 1, "statuses": []map[string]any{{"id": 2, "context": "build", "state": "success", "updated_at": testTime.Add(-time.Second)}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	source, err := New(Config{Name: "review-pr", Repository: "owner/repo", PullRequest: 7, APIBaseURL: server.URL, Token: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestCollectorExactSnapshotStableIdentityAndFreshObservation(t *testing.T) {
	source := setup(t, nil)
	clock := testTime
	source.clock = func() time.Time { return clock }
	first, err := source.CollectAutomationFacts(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Facts) != 2 || first.Facts[0].Value != "open" || first.Facts[1].Value != "succeeded" {
		t.Fatalf("unexpected facts: %+v", first)
	}
	next := request()
	next.Now = next.Now.Add(time.Minute)
	clock = next.Now
	next.Cursor = first.NextCursor
	second, err := source.CollectAutomationFacts(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if second.NextCursor != first.NextCursor || second.Facts[1].EventID != first.Facts[1].EventID || !second.Facts[1].ObservedAt.Equal(next.Now) {
		t.Fatal("snapshot identity or freshness changed incorrectly")
	}
}

func TestCollectorRejectsPartialAndChangedSnapshots(t *testing.T) {
	for _, scenario := range []string{"wrong-target", "wrong-head", "missing-page", "bad-json", "authorization", "head-moved", "too-many"} {
		t.Run(scenario, func(t *testing.T) {
			pulls := 0
			source := setup(t, func(w http.ResponseWriter, r *http.Request) bool {
				if strings.Contains(r.URL.Path, "/pulls/") {
					pulls++
					if scenario == "head-moved" && pulls == 2 {
						respondJSON(w, pullJSON(strings.Repeat("b", 40)))
						return true
					}
				}
				if strings.HasSuffix(r.URL.Path, "/status") && scenario == "wrong-head" {
					respondJSON(w, map[string]any{"sha": strings.Repeat("b", 40), "total_count": 0, "statuses": []any{}})
					return true
				}
				if !strings.HasSuffix(r.URL.Path, "/check-runs") {
					return false
				}
				switch scenario {
				case "missing-page":
					respondJSON(w, map[string]any{"total_count": 2, "check_runs": []any{}})
					return true
				case "too-many":
					respondJSON(w, map[string]any{"total_count": 501, "check_runs": []any{}})
					return true
				case "bad-json":
					_, _ = fmt.Fprint(w, "{bad")
					return true
				case "authorization":
					http.Error(w, "fixture-token must never reach errors", http.StatusForbidden)
					return true
				}
				return false
			})
			req := request()
			if scenario == "wrong-target" {
				req.Resource.PullRequest = 8
			}
			result, err := source.CollectAutomationFacts(context.Background(), req)
			if err == nil || len(result.Facts) != 0 {
				t.Fatalf("partial snapshot accepted: %+v %v", result, err)
			}
			if strings.Contains(err.Error(), "fixture-token") {
				t.Fatal("credential leaked into error")
			}
			if scenario == "wrong-target" && pulls != 0 {
				t.Fatal("mismatched target issued a request")
			}
		})
	}
}

func TestCollectorDoesNotFollowRedirects(t *testing.T) {
	reached := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; http.Error(w, "unexpected", 500) }))
	defer other.Close()
	source := setup(t, func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, other.URL, http.StatusFound)
		return true
	})
	if _, err := source.CollectAutomationFacts(context.Background(), request()); err == nil || reached {
		t.Fatal("source redirect followed or accepted")
	}
}

func TestCIRequiresCompleteObservedResults(t *testing.T) {
	done := check{ID: 1, Status: "completed", Conclusion: "success", CompletedAt: testTime}
	for _, tc := range []struct {
		name     string
		checks   []check
		statuses []status
		want     string
	}{
		{name: "absent", want: "unknown"},
		{name: "success", checks: []check{done}, want: "succeeded"},
		{name: "failure", checks: []check{{ID: 1, Status: "completed", Conclusion: "failure", CompletedAt: testTime}}, want: "failed"},
		{name: "unfinished-after-failure", checks: []check{{ID: 1, Status: "completed", Conclusion: "failure", CompletedAt: testTime}, {ID: 2, Status: "in_progress"}}, want: "pending"},
		{name: "status-pending", checks: []check{done}, statuses: []status{{State: "pending", UpdatedAt: testTime}}, want: "pending"},
		{name: "unrecognized-conclusion", checks: []check{{Status: "completed", Conclusion: "new_upstream_value", CompletedAt: testTime}}, want: "unknown"},
		{name: "missing-completion-time", checks: []check{{Status: "completed", Conclusion: "success"}}, want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := ciState(tc.checks, tc.statuses)
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCollectorValidation(t *testing.T) {
	for _, repo := range []string{"../repo", "owner/..", "owner/repo?redirect=bad", "owner/repo/extra"} {
		if _, err := New(Config{Name: "source", Repository: repo, PullRequest: 1}); err == nil {
			t.Fatalf("accepted %q", repo)
		}
	}
	if _, err := New(Config{Name: "source", Repository: "owner/repo", PullRequest: 1, APIBaseURL: "http://example.com"}); err == nil {
		t.Fatal("accepted non-TLS remote")
	}
}

func TestCollectorPollingBoundDoesNotRefreshCachedEvidence(t *testing.T) {
	calls := 0
	source := setup(t, func(http.ResponseWriter, *http.Request) bool { calls++; return false })
	first, err := source.CollectAutomationFacts(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 60; i++ {
		req := request()
		req.Now = req.Now.Add(time.Duration(i) * time.Second)
		batch, err := source.CollectAutomationFacts(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if !batch.Facts[0].ObservedAt.Equal(first.Facts[0].ObservedAt) {
			t.Fatal("cached evidence refreshed")
		}
		batch.Facts[0].Value = "caller mutation"
	}
	if calls != 4 {
		t.Fatalf("made %d reads inside one polling window", calls)
	}
	req := request()
	req.Now = req.Now.Add(time.Minute)
	again, err := source.CollectAutomationFacts(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 8 || again.Facts[0].Value != "open" {
		t.Fatal("polling or response isolation failed")
	}
}
