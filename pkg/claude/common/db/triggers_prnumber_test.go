package db

import "testing"

func TestDerivePRNumber(t *testing.T) {
	cases := map[string]int{
		"https://github.com/tofutools/tclaude/pull/42":                             42,
		"https://gitlab.com/acme/app/-/merge_requests/7":                           7,
		"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/157": 157,
		"https://github.com/tofutools/tclaude/issues/42":                           0,
		"https://gitlab.com/acme/app/-/issues/7":                                   0,
		"https://example.com/review/7":                                             0,
		"https://gitlab.com/acme/app/-/merge_requests/7suffix":                     0,
		"https://example.com/team/app/merge_requests/7":                            0,
		"https://github.com/tofutools/tclaude/pull/42abc":                          0,
		"https://github.com/tofutools/tclaude/pull/0":                              0,
		"https://gitlab.com/acme/app/merge_requests/7":                             0,
		"https://gitlab.com/acme/-/merge_requests/7":                               0, // no project segment
		"": 0,
	}
	for raw, want := range cases {
		if got := derivePRNumber(raw); got != want {
			t.Errorf("derivePRNumber(%q) = %d, want %d", raw, got, want)
		}
	}
}
