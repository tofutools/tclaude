package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeAgentTag(t *testing.T) {
	ok, err := NormalizeAgentTag("  tf:squad  ")
	require.NoError(t, err)
	assert.Equal(t, "tf:squad", ok, "trims surrounding whitespace")

	_, err = NormalizeAgentTag("   ")
	assert.Error(t, err, "empty-after-trim rejected")

	_, err = NormalizeAgentTag("bad\ntag")
	assert.Error(t, err, "newline rejected")

	_, err = NormalizeAgentTag("ctrl\x07tag")
	assert.Error(t, err, "control char rejected")

	_, err = NormalizeAgentTag("a,b")
	assert.Error(t, err, "comma rejected (dashboard separator)")

	_, err = NormalizeAgentTag(strings.Repeat("x", MaxAgentTagLen+1))
	assert.Error(t, err, "over-length rejected")

	ok, err = NormalizeAgentTag(strings.Repeat("x", MaxAgentTagLen))
	require.NoError(t, err, "exactly max is allowed")
	assert.Len(t, ok, MaxAgentTagLen)
}

func TestTaskForceTag(t *testing.T) {
	assert.Equal(t, "", TaskForceTag("  "), "blank name yields no tag")
	assert.Equal(t, "tf:squad", TaskForceTag("squad"), "short name kept verbatim")
	// Comma + control chars are stripped so the auto-stamp always validates.
	assert.Equal(t, "tf:ab", TaskForceTag("a,b"), "comma stripped")
	assert.Equal(t, "tf:ab", TaskForceTag("a\nb"), "newline stripped")
	// A long name is truncated so tf:<name> always fits the length cap, and
	// the result is a VALID tag (NormalizeAgentTag accepts it).
	long := TaskForceTag(strings.Repeat("x", MaxAgentTagLen*2))
	assert.LessOrEqual(t, len([]rune(long)), MaxAgentTagLen, "truncated to the cap")
	assert.Equal(t, TaskForceTagPrefix, long[:3], "prefix preserved")
	norm, err := NormalizeAgentTag(long)
	require.NoError(t, err, "the truncated auto-stamp is a valid tag")
	assert.Equal(t, long, norm)
}

func TestAgentTags_AddReplaceRemoveList(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
		VALUES ('agt_a', 'conv-a', '2026-07-04T00:00:00Z')`)

	// Add is additive + sorted, and de-dupes against existing.
	require.NoError(t, AddAgentTags("agt_a", "b", "a"))
	require.NoError(t, AddAgentTags("agt_a", "a", "c")) // "a" already present
	tags, err := ListAgentTags("agt_a")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, tags)

	// Remove drops only the named tags; missing tags are ignored.
	require.NoError(t, RemoveAgentTags("agt_a", "b", "zzz"))
	tags, _ = ListAgentTags("agt_a")
	assert.Equal(t, []string{"a", "c"}, tags)

	// Replace sets the set exactly (delete-all + insert).
	require.NoError(t, ReplaceAgentTags("agt_a", []string{"x", "y", "x"}))
	tags, _ = ListAgentTags("agt_a")
	assert.Equal(t, []string{"x", "y"}, tags, "replace de-dupes and sorts")

	// Replace with empty clears.
	require.NoError(t, ReplaceAgentTags("agt_a", nil))
	tags, _ = ListAgentTags("agt_a")
	assert.Empty(t, tags)
}

func TestAgentTags_RejectsInvalidAndOverCap(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
		VALUES ('agt_b', 'conv-b', '2026-07-04T00:00:00Z')`)

	assert.Error(t, AddAgentTags("agt_b", "bad\ntag"), "newline rejected on add")
	assert.Error(t, ReplaceAgentTags("agt_b", []string{"ok", "  "}), "empty rejected on replace")

	// Over the per-agent count cap.
	over := make([]string, MaxAgentTags+1)
	for i := range over {
		over[i] = "tag" + string(rune('a'+i))
	}
	assert.Error(t, ReplaceAgentTags("agt_b", over), "over-cap replace rejected")

	// Add that would push past the cap.
	full := make([]string, MaxAgentTags)
	for i := range full {
		full[i] = "t" + string(rune('a'+i))
	}
	require.NoError(t, ReplaceAgentTags("agt_b", full), "exactly the cap is allowed")
	assert.Error(t, AddAgentTags("agt_b", "one-too-many"), "add past cap rejected")
	// A no-op re-add of an existing tag never trips the cap.
	require.NoError(t, AddAgentTags("agt_b", full[0]), "re-adding an existing tag at cap is a no-op")
}

func TestListAllAgentTags(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	for _, id := range []string{"agt_x", "agt_y", "agt_z"} {
		mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
			VALUES (?, ?, '2026-07-04T00:00:00Z')`, id, "conv-"+id)
	}
	require.NoError(t, AddAgentTags("agt_x", "b", "a"))
	require.NoError(t, AddAgentTags("agt_y", "solo"))
	// agt_z left tagless — must be omitted from the map.

	all, err := ListAllAgentTags()
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, all["agt_x"], "sorted per agent")
	assert.Equal(t, []string{"solo"}, all["agt_y"])
	_, present := all["agt_z"]
	assert.False(t, present, "tagless agent omitted")
}
