package agentd

import "testing"

// TestOriginMatchesBase nails the loopback origin pin shared by the
// dashboard /api, the dashboard login POST, and the approval popup. The
// load-bearing case is the port-superstring rejection: a bare
// strings.HasPrefix accepted "http://127.0.0.1:6553" against base
// "...:655", letting a hostile same-user process on a different (valid)
// port clear the pin.
func TestOriginMatchesBase(t *testing.T) {
	const base = "http://127.0.0.1:655"
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"exact origin", "http://127.0.0.1:655", true},
		{"referer with path", "http://127.0.0.1:655/", true},
		{"referer deep path", "http://127.0.0.1:655/dashboard/login", true},
		{"port superstring origin", "http://127.0.0.1:6553", false},
		{"port superstring referer", "http://127.0.0.1:6553/x", false},
		{"different port", "http://127.0.0.1:9999", false},
		{"different host", "http://10.0.0.5:655", false},
		{"different scheme", "https://127.0.0.1:655", false},
		{"evil suffix host", "http://127.0.0.1:655.evil.com", false},
		{"empty value", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := originMatchesBase(tc.value, base); got != tc.want {
				t.Errorf("originMatchesBase(%q, %q) = %v, want %v", tc.value, base, got, tc.want)
			}
		})
	}

	// Empty base is a defensive default of the pure predicate: callers
	// (checkDashboardAuth / checkLoginOrigin / checkPopupAuth) gate on
	// popupBaseURL != "" and skip the pin entirely when no listener is
	// bound, so they never reach here with an empty base.
	if originMatchesBase("http://127.0.0.1:655", "") {
		t.Error("originMatchesBase with empty base must be false")
	}
}
