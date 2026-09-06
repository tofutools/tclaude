package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type dashboardGitBatch struct {
	Requests    []dashboardGitRequest `json:"requests"`
	Concurrency int                   `json:"concurrency"`
}

// Only the caller writes to the response. Workers send progress through a
// bounded channel; disconnecting cancels queued work and active Git commands.
func streamDashboardGitBatch(ctx context.Context, requests []dashboardGitRequest, concurrency int, run func(context.Context, dashboardGitRequest) dashboardGitResult, emit func(dashboardGitResult) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan dashboardGitResult)
	jobs := make(chan dashboardGitRequest)
	send := func(result dashboardGitResult) bool {
		select {
		case events <- result:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var workers sync.WaitGroup
	for range min(concurrency, len(requests)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for request := range jobs {
				if ctx.Err() != nil {
					return
				}
				if !send(dashboardGitResult{Path: request.Path, Status: "running"}) {
					return
				}
				child, stop := context.WithTimeout(ctx, 2*time.Minute)
				result := run(child, request)
				stop()
				if !send(result) {
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, request := range requests {
			select {
			case jobs <- request:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { workers.Wait(); close(events) }()
	// Drain after a write error so canceled workers have exited before returning.
	var writeErr error
	for event := range events {
		if writeErr == nil {
			writeErr = emit(event)
			if writeErr != nil {
				cancel()
			}
		}
	}
	if writeErr != nil {
		return writeErr
	}
	return ctx.Err()
}

func handleDashboardGitBatch(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var batch dashboardGitBatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024)).Decode(&batch); err != nil {
		http.Error(w, "invalid batch", http.StatusBadRequest)
		return
	}
	if len(batch.Requests) == 0 || len(batch.Requests) > 1000 || batch.Concurrency < 1 || batch.Concurrency > 100 {
		http.Error(w, "batch requires 1–1000 repositories and concurrency 1–100", http.StatusBadRequest)
		return
	}
	group := batch.Requests[0].Group
	for _, request := range batch.Requests {
		if request.Group != group || request.Path == "" || (request.Mode != "pull" && request.Mode != "sync") {
			http.Error(w, "batch requires one group scope, repository paths and pull/sync mode", http.StatusBadRequest)
			return
		}
	}
	// Revalidate scope once for the entire batch, not once per repository.
	discovery, stop := context.WithTimeout(r.Context(), 2*time.Minute)
	repos, _, err := dashboardGitHomes(discovery, group, "")
	stop()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	allowed := make(map[string]bool, len(repos))
	for _, repo := range repos {
		allowed[repo.Path] = true
	}
	canonical := make(map[string]string, len(batch.Requests))
	seen := make(map[string]bool, len(batch.Requests))
	for _, request := range batch.Requests {
		path, err := canonicalDashboardGitPath(request.Path)
		if err != nil {
			path = request.Path
		}
		if seen[path] {
			http.Error(w, "duplicate repository in batch", http.StatusBadRequest)
			return
		}
		seen[path] = true
		if err == nil && allowed[path] {
			canonical[request.Path] = path
		}
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		return
	}
	encoder := json.NewEncoder(w)
	emit := func(result dashboardGitResult) error {
		if err := encoder.Encode(result); err != nil {
			return err
		}
		return controller.Flush()
	}
	run := func(ctx context.Context, request dashboardGitRequest) dashboardGitResult {
		original := request.Path
		path, ok := canonical[original]
		if !ok {
			return dashboardGitResult{Path: original, Status: "skipped", Detail: "Repository is unavailable or no longer in scope; rescan"}
		}
		// A path may have been retargeted while this job was queued.
		current, err := canonicalDashboardGitPath(original)
		if err != nil || current != path {
			return dashboardGitResult{Path: original, Status: "skipped", Detail: "Repository path changed; rescan"}
		}
		request.Path = path
		result := runDashboardGit(ctx, request)
		result.Path = original
		return result
	}
	if err := streamDashboardGitBatch(r.Context(), batch.Requests, batch.Concurrency, run, emit); err != nil {
		return
	}
	if err := emit(dashboardGitResult{Status: "complete", Detail: fmt.Sprintf("%d repositories processed", len(batch.Requests))}); err != nil {
		return
	}
}
