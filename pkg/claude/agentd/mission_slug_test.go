package agentd

import "testing"

// JOH-344 #3: a bare issue-tracker URL should name the deployed force after its
// issue key, not fall back to the template name (which makes three deploys of
// dev-squad against three issues indistinguishable). slugForMission is the seam
// that turns the mission into the group-name base.
func TestSlugForMission(t *testing.T) {
	cases := []struct {
		name    string
		mission string
		want    string
	}{
		{"bare Linear URL with key", "https://linear.app/acme/issue/JOH-245/some-title", "joh-245"},
		{"scheme-less Linear URL with key", "linear.app/acme/issue/JOH-245/some-title", "joh-245"},
		{"key wins over key-shaped org slug", "https://linear.app/acme-2/issue/JOH-9/title", "joh-9"},
		{"bare URL, query + fragment stripped", "https://linear.app/acme/issue/ABC-12?x=1#frag", "abc-12"},
		{"GitHub issues URL has no letters-dashed key", "https://github.com/tofutools/tclaude/issues/123", ""},
		{"bare URL with no recognizable key", "https://example.com/some/path", ""},
		{"free-text mission slugs the text", "Ship the new billing flow", "ship-the-new-billing-flow"},
		// A mission that merely contains a URL still slugs the whole text (the
		// URL collapses to dashes); the result is capped at slugify's 40 bytes.
		{"text containing a URL slugs the text", "See https://linear.app/acme/issue/JOH-1 for details",
			"see-https-linear-app-acme-issue-joh-1-fo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := slugForMission(tc.mission); got != tc.want {
				t.Fatalf("slugForMission(%q) = %q, want %q", tc.mission, got, tc.want)
			}
		})
	}
}

// issueKeyFromURL is the generic key extractor slugForMission leans on. It is
// deliberately not Linear-specific — any <letters>-<digits> path segment reads
// as a key, with a segment following "issue"/"issues" preferred.
func TestIssueKeyFromURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"linear.app/acme/issue/JOH-245/title", "joh-245"},
		{"https://linear.app/acme/issue/JOH-245", "joh-245"},
		{"https://linear.app/acme-2/issue/JOH-9/x", "joh-9"},   // org slug "acme-2" is key-shaped; the /issue/ one wins
		{"https://tracker.example/browse/PROJ-42", "proj-42"},  // no "issue" segment → first key-shaped segment
		{"https://github.com/tofutools/tclaude/issues/123", ""}, // number is not letters-dashed
		{"https://example.com/no/key/here", ""},
	}
	for _, tc := range cases {
		if got := issueKeyFromURL(tc.in); got != tc.want {
			t.Errorf("issueKeyFromURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
