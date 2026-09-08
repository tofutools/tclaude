package model

// TeamRhythm becomes an ordinary schedule for the deployed group. An empty
// RoleID targets all current members; otherwise membership is filtered at fire.
type TeamRhythm struct {
	Name     string
	RoleID   RoleID `json:",omitempty"`
	Interval string `json:",omitempty"`
	Cron     string `json:",omitempty"`
	Timezone string
	Subject  string `json:",omitempty"`
	Body     string
}
