package agentd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTrackedPRSearchArgs(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	argv := strings.Join(trackedPRSearchArgs([]string{"alice", "bob"}, trackedPROpenLimit, 3, now, true), "\n")
	assert.Contains(t, argv, "query($first:Int!,$rfirst:Int!,$q0:String!,$qr0:String!,$q1:String!,$qr1:String!){")
	assert.Contains(t, argv, "r1: search(query:$qr1,")
	assert.Contains(t, argv, "qr0=author:alice is:closed type:pr closed:>=2026-10-17")
	assert.Equal(t, 2, strings.Count(argv, "latestOpinionatedReviews"), "reviewers ride every author's open search")

	argv = strings.Join(trackedPRSearchArgs([]string{"alice"}, trackedPRDegradedOpenLimit, 0, now, false), "\n")
	assert.NotContains(t, argv, "rfirst", "no closed search while that window is off")
	assert.NotContains(t, argv, "latestOpinionatedReviews")
	assert.Contains(t, argv, "first=20")
}

func TestNormalizeTrackedPRAuthors(t *testing.T) {
	got := normalizeTrackedPRAuthors([]string{"alice", "Alice", "bad login", "", "b", "c", "d", "e", "f", "g", "h", "i"})
	assert.Equal(t, []string{"alice", "b", "c", "d", "e", "f", "g", "h"}, got)
}

func TestDecodeTrackedPRGraphQLFailureShapes(t *testing.T) {
	authors := []string{"alice", "bob"}
	// gh prints an HTTP error body to stdout: no data must fail the tick, not
	// blank every tab.
	_, err := decodeTrackedPRGraphQL([]byte(`{"message":"Bad credentials"}`), authors, trackedPROpenLimit, 0)
	assert.Error(t, err)
	_, err = decodeTrackedPRGraphQL([]byte(`{"data":null,"errors":[{"path":["u0","nodes"],"message":"boom"}]}`), authors, trackedPROpenLimit, 0)
	assert.Error(t, err)
	// A missing result for an author nobody refused is a failure too.
	_, err = decodeTrackedPRGraphQL([]byte(`{"data":{"u0":{"issueCount":0,"nodes":[]}}}`), authors, trackedPROpenLimit, 0)
	assert.Error(t, err)

	// An error deep inside one PR leaves the rest of that author's list.
	views, err := decodeTrackedPRGraphQL([]byte(`{"data":{
		"u0":{"issueCount":1,"nodes":[{"number":3,"title":"t","url":"https://github.com/a/b/pull/3","repository":{"nameWithOwner":"a/b"},"commits":{"nodes":[]}}]},
		"u1":null},
		"errors":[
			{"path":["u0","nodes",0,"commits"],"message":"SAML enforcement"},
			{"path":["u1"],"message":"cannot be searched"}]}`), authors, trackedPROpenLimit, 0)
	assert.NoError(t, err)
	if assert.Len(t, views, 2) {
		assert.Empty(t, views[0].Error)
		assert.Len(t, views[0].Items, 1)
		assert.Equal(t, "cannot be searched", views[1].Error)
	}
}

func TestTrackedPRResolverKeepsGHCause(t *testing.T) {
	prev := trackedPRGraphQL
	t.Cleanup(func() { trackedPRGraphQL = prev })
	cause := subprocessError(context.Background(), time.Second, errors.New("exit status 1"),
		"gh: HTTP 502", []byte("<html><title>Unicorn! &middot; GitHub</title></html>"))
	trackedPRGraphQL = func([]string) ([]byte, error) {
		return []byte("<html><title>Unicorn! &middot; GitHub</title></html>"), cause
	}
	results := liveTrackedPRResolver([]string{"alice", "bob"}, map[string]int{"alice": 50, "bob": 50}, 0, time.Now(), false, func() bool { return true })
	if assert.Len(t, results, 2) {
		assert.Equal(t, "bob", results[1].view.Login)
		assert.EqualError(t, results[1].err, "gh: HTTP 502; response: Unicorn! · GitHub: exit status 1",
			"an undecodable body reports gh's cause, not a bare exit status")
	}
}

func TestTrackedPRStartPacer(t *testing.T) {
	// No spacing: never blocks.
	assert.True(t, trackedPRStartPacer(0, nil)())

	// A kick ends the spacing for the rest of the tick and stays queued.
	for len(trackedPRKick) > 0 {
		<-trackedPRKick
	}
	t.Cleanup(func() {
		for len(trackedPRKick) > 0 {
			<-trackedPRKick
		}
	})
	pace := trackedPRStartPacer(time.Hour, nil)
	kickTrackedPRAuthorsPoller()
	assert.True(t, pace())
	assert.True(t, pace(), "no further waiting once kicked")
	assert.Len(t, trackedPRKick, 1, "the kick is re-queued for the next tick")

	// Stop skips the remaining requests.
	<-trackedPRKick
	stop := make(chan struct{})
	close(stop)
	assert.False(t, trackedPRStartPacer(time.Hour, stop)())
}

func TestTrackedPRSlowAuthors(t *testing.T) {
	t.Cleanup(func() {
		trackedPRSlowAuthors.Lock()
		clear(trackedPRSlowAuthors.byLogin)
		trackedPRSlowAuthors.Unlock()
	})
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	authors := []string{"alice", "Bob"}
	timeout := errors.New("gh: HTTP 502: exit status 1")

	due, limits := planTrackedPRSearches(authors, now)
	assert.Equal(t, authors, due)
	assert.Equal(t, map[string]int{"alice": trackedPROpenLimit, "Bob": trackedPROpenLimit}, limits)

	// One timeout: still searched, with the smaller page.
	recordTrackedPRSearch("Bob", timeout, now)
	recordTrackedPRSearch("alice", errors.New("gh: Could not resolve to a User"), now)
	due, limits = planTrackedPRSearches(authors, now.Add(time.Minute))
	assert.Equal(t, authors, due, "a non-timeout failure changes nothing")
	assert.Equal(t, trackedPRDegradedOpenLimit, limits["Bob"])
	assert.Equal(t, trackedPROpenLimit, limits["alice"])

	// A second timeout in a row defers Bob's next search.
	recordTrackedPRSearch("Bob", timeout, now.Add(time.Minute))
	due, _ = planTrackedPRSearches(authors, now.Add(2*time.Minute))
	assert.Equal(t, []string{"alice"}, due)
	due, limits = planTrackedPRSearches(authors, now.Add(4*time.Minute))
	assert.Equal(t, authors, due, "the deferral expires")

	// A success ends the streak but keeps the smaller page for a while.
	recordTrackedPRSearch("Bob", nil, now.Add(4*time.Minute))
	recordTrackedPRSearch("Bob", timeout, now.Add(5*time.Minute))
	due, limits = planTrackedPRSearches(authors, now.Add(6*time.Minute))
	assert.Equal(t, authors, due, "one timeout after a success does not defer")
	assert.Equal(t, trackedPRDegradedOpenLimit, limits["Bob"])
	due, limits = planTrackedPRSearches(authors, now.Add(5*time.Minute+trackedPRDegradedFor))
	assert.Equal(t, trackedPROpenLimit, limits["Bob"], "the full page comes back")

	// Removing an author forgets them.
	planTrackedPRSearches([]string{"alice"}, now)
	trackedPRSlowAuthors.Lock()
	assert.Empty(t, trackedPRSlowAuthors.byLogin)
	trackedPRSlowAuthors.Unlock()
}
