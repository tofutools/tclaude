package agentd

import (
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
