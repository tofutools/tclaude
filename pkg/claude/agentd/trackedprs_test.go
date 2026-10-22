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
	argv := strings.Join(trackedPRSearchArgs([]string{"alice", "bob"}, 3, now, true), "\n")
	assert.Contains(t, argv, "query($first:Int!,$rfirst:Int!,$q0:String!,$qr0:String!,$q1:String!,$qr1:String!){")
	assert.Contains(t, argv, "r1: search(query:$qr1,")
	assert.Contains(t, argv, "qr0=author:alice is:closed type:pr closed:>=2026-10-17")
	assert.Equal(t, 2, strings.Count(argv, "latestOpinionatedReviews"), "reviewers ride every author's open search")

	argv = strings.Join(trackedPRSearchArgs([]string{"alice"}, 0, now, false), "\n")
	assert.NotContains(t, argv, "rfirst", "no closed search while that window is off")
	assert.NotContains(t, argv, "latestOpinionatedReviews")
}

func TestNormalizeTrackedPRAuthors(t *testing.T) {
	got := normalizeTrackedPRAuthors([]string{"alice", "Alice", "bad login", "", "b", "c", "d", "e", "f", "g", "h", "i"})
	assert.Equal(t, []string{"alice", "b", "c", "d", "e", "f", "g", "h"}, got)
}

func TestDecodeTrackedPRGraphQLFailureShapes(t *testing.T) {
	authors := []string{"alice", "bob"}
	// gh prints an HTTP error body to stdout: no data must fail the tick, not
	// blank every tab.
	_, err := decodeTrackedPRGraphQL([]byte(`{"message":"Bad credentials"}`), authors, 0)
	assert.Error(t, err)
	_, err = decodeTrackedPRGraphQL([]byte(`{"data":null,"errors":[{"path":["u0","nodes"],"message":"boom"}]}`), authors, 0)
	assert.Error(t, err)
	// A missing result for an author nobody refused is a failure too.
	_, err = decodeTrackedPRGraphQL([]byte(`{"data":{"u0":{"issueCount":0,"nodes":[]}}}`), authors, 0)
	assert.Error(t, err)

	// An error deep inside one PR leaves the rest of that author's list.
	views, err := decodeTrackedPRGraphQL([]byte(`{"data":{
		"u0":{"issueCount":1,"nodes":[{"number":3,"title":"t","url":"https://github.com/a/b/pull/3","repository":{"nameWithOwner":"a/b"},"commits":{"nodes":[]}}]},
		"u1":null},
		"errors":[
			{"path":["u0","nodes",0,"commits"],"message":"SAML enforcement"},
			{"path":["u1"],"message":"cannot be searched"}]}`), authors, 0)
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
	_, err := liveTrackedPRResolver([]string{"alice", "bob"}, 0, time.Now(), false)
	assert.EqualError(t, err, "gh: HTTP 502; response: Unicorn! &middot; GitHub: exit status 1",
		"an undecodable body reports gh's cause, not a bare exit status")
}
