package db

import "encoding/json"

// Rhythm is one template-declared recurring nudge (JOH-244): the party's
// "drumbeat". At deploy it is materialized as a normal cron job targeting the
// instantiated group, role-filtered at fire time — e.g. "PO pings the workers
// every 10 min", "the lead posts a status hourly". After materialization it is
// an ordinary cron job: visible in `cron ls` + the dashboard, editable and
// removable by the human like any other.
//
// Exactly one of IntervalSeconds / CronExpr sets the schedule (the same
// exclusive pair a cron job carries). TargetRole is a role label the nudge is
// filtered to at fire time (matched case-insensitively against a live member's
// role, the work-pattern / process rule); "" or the literal "all" means the
// whole group. Subject is optional; Body is the message text.
type Rhythm struct {
	Name            string `json:"name"`
	TargetRole      string `json:"target_role,omitempty"`
	IntervalSeconds int64  `json:"interval_seconds,omitempty"`
	CronExpr        string `json:"cron_expr,omitempty"`
	Subject         string `json:"subject,omitempty"`
	Body            string `json:"body"`
}

// rhythmsToJSON marshals a rhythm list for the group_templates.rhythms TEXT
// column. An empty list stores as "[]" (the processToJSON / workPatternToJSON
// convention) so a reader can json.Unmarshal it unconditionally; legacy rows
// hold '' and read back as empty.
func rhythmsToJSON(rhythms []Rhythm) string {
	if len(rhythms) == 0 {
		return "[]"
	}
	b, err := json.Marshal(rhythms)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// rhythmsFromJSON parses the rhythms TEXT column back into a slice. A blank
// ('' — pre-v93 template rows) or malformed value yields an empty (non-nil)
// slice.
func rhythmsFromJSON(s string) []Rhythm {
	out := []Rhythm{}
	if s == "" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []Rhythm{}
	}
	return out
}
