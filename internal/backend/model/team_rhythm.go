package model

// TeamRhythm becomes an ordinary schedule for the deployed group. An empty
// role target selects all current members; otherwise labels are resolved at fire.
// RoleID remains available for previously authored permission-role filters.
type TeamRhythm struct {
	RoleLabel string `json:",omitempty"`
	Name      string
	RoleID    RoleID `json:",omitempty"`
	Interval  string `json:",omitempty"`
	Cron      string `json:",omitempty"`
	Timezone  string
	Subject   string `json:",omitempty"`
	Body      string
}
