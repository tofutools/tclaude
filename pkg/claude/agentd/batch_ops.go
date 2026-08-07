package agentd

import (
	"errors"

	"golang.org/x/sync/errgroup"
)

// Shared fan-out for the daemon's bulk agent operations.
//
// Every "act on a cohort of agents" endpoint — group stop, group resume,
// group retire, the dashboard's cleanup tiers (unjoin / retire / delete /
// reinstate), the power buttons — used to hand-roll its own loop, and most of
// them rolled a SEQUENTIAL one. That is what the operator sees as a bulk
// retire trickling one or two agents at a time behind a spinning dialog: the
// per-agent work is dominated by subprocess latency (a tmux liveness probe, a
// send-keys, a spawn, and now a wait for the pane to actually die), so a
// sequential loop over N agents costs N × that latency for no reason.
//
// The per-agent work in all of these is independent — each target's DB writes
// touch its own rows, and the shared inputs (worktree claim snapshots, owner
// rosters, liveness maps) are resolved ONCE before the fan-out precisely so
// workers never race on them. So the shape is always the same: run the
// per-agent step concurrently under a small bound, write each result into its
// own pre-sized slot, then fold the slots into counters/warnings sequentially
// once every worker has settled. These helpers are that shape, so the callers
// stop re-deriving it.
//
// Ordering is preserved: results come back in input order regardless of how
// the goroutines interleaved, so responses (and the flow tests that read them)
// stay deterministic.

// batchAgentOpConcurrency bounds how many agents a bulk operation acts on at
// once.
//
// The per-agent step is I/O-bound — tmux probes and send-keys, SQLite writes,
// occasionally a spawned subprocess — so a handful of workers overlaps that
// latency without stampeding tmux or the single SQLite writer (WAL serialises
// writes; busy_timeout absorbs the contention). It matches the bound the group
// retire path already chose (bulkRetireGroupConcurrency) so every bulk surface
// behaves the same under load.
const batchAgentOpConcurrency = 8

// mapAgentsConcurrently applies fn to every item under a bounded worker pool
// and returns the kept results in INPUT order.
//
// fn reports (result, keep): keep=false omits the item from the output
// entirely, which is how the cohort-narrowing paths (a member filtered out by
// ?status=, a conv absent from an explicit selection) drop a target without
// leaving a hole in the response table. Each worker writes only its own slot,
// so fn needs no locking for the value it returns — but anything fn touches
// OUTSIDE that value must be safe for concurrent use or resolved before the
// call.
func mapAgentsConcurrently[T, R any](items []T, limit int, fn func(i int, item T) (R, bool)) []R {
	if len(items) == 0 {
		return nil
	}
	type slot struct {
		val  R
		keep bool
	}
	slots := make([]slot, len(items))
	forEachAgentConcurrently(items, limit, func(i int, item T) {
		slots[i].val, slots[i].keep = fn(i, item)
	})
	out := make([]R, 0, len(items))
	for _, s := range slots {
		if s.keep {
			out = append(out, s.val)
		}
	}
	return out
}

// forEachAgentConcurrently runs fn for every item under a bounded worker pool
// and returns once every worker has settled. limit <= 0 means
// batchAgentOpConcurrency.
//
// It is the primitive under mapAgentsConcurrently, exposed for callers that
// write into several parallel result slices at once (per-target outcome plus
// the group ids its work touched, say) rather than a single value.
func forEachAgentConcurrently[T any](items []T, limit int, fn func(i int, item T)) {
	if len(items) == 0 {
		return
	}
	if limit <= 0 {
		limit = batchAgentOpConcurrency
	}
	if limit > len(items) {
		limit = len(items)
	}
	if limit == 1 {
		for i, item := range items {
			fn(i, item)
		}
		return
	}
	eg := new(errgroup.Group)
	eg.SetLimit(limit)
	for i, item := range items {
		eg.Go(func() error {
			fn(i, item)
			return nil
		})
	}
	_ = eg.Wait() // fn never returns an error — per-item failures ride in its result
}

// joinAgentOpErrors runs fn for every item concurrently and joins whatever the
// workers returned into ONE error, in input order, with the nils dropped.
//
// This is the variant for bulk work whose per-item failure is a plain error
// rather than an outcome row: the caller gets a single error that names every
// target that failed instead of aborting on the first one. Returns nil when
// every worker succeeded.
func joinAgentOpErrors[T any](items []T, limit int, fn func(i int, item T) error) error {
	if len(items) == 0 {
		return nil
	}
	errs := make([]error, len(items))
	forEachAgentConcurrently(items, limit, func(i int, item T) {
		errs[i] = fn(i, item)
	})
	return errors.Join(errs...)
}
