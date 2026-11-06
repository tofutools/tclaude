package agent

// memberResultsGolden is the output the member-result commands printed
// before their table was shared (TestMemberResultsGolden).
var memberResultsGolden = map[string]string{
	"groups retire/members":         "Group \"crew\" — retired:\nID            NAME       ACTION      DETAIL         \n────────────────────────────────────────────────────\nagt_12345678  builder    retired     pane closed    \nc2b4e6f8      (unnamed)  skipped     already offline\n⚠ one member was already gone\n",
	"groups retire/empty":           "Group \"crew\" has no members.\n",
	"groups stop/members":           "Group \"crew\" — retired:\nID            NAME       ACTION      DETAIL         \n────────────────────────────────────────────────────\nagt_12345678  builder    retired     pane closed    \nc2b4e6f8      (unnamed)  skipped     already offline\nWarning (c2b4e6f8): no worktree\nChanged since last launch (c2b4e6f8): model\n",
	"groups stop/empty":             "Group \"crew\" has no members.\n",
	"groups resume/members":         "Group \"crew\" — retired:\nID            NAME       ACTION      DETAIL         \n────────────────────────────────────────────────────\nagt_12345678  builder    retired     pane closed    \nc2b4e6f8      (unnamed)  skipped     already offline\nWarning (c2b4e6f8): no worktree\nChanged since last launch (c2b4e6f8): model\n",
	"groups resume/empty":           "Group \"crew\" has no members.\n",
	"task-force stand-down/members": "Task force \"crew\" stood down:\nID            NAME       ACTION      DETAIL         \n────────────────────────────────────────────────────\nagt_12345678  builder    retired     pane closed    \nc2b4e6f8      (unnamed)  skipped     already offline\nSwept: 2 rhythm jobs removed, 1 pending wave cancelled.\nThe group is kept as a dormant record (mission + history preserved). Use `tclaude agent groups rm crew` to delete it entirely.\n⚠ one member was already gone\n",
	"task-force stand-down/empty":   "Task force \"crew\" stood down:\n  (no members to retire)\nSwept: 0 rhythm jobs removed, 0 pending waves cancelled.\nThe group is kept as a dormant record (mission + history preserved). Use `tclaude agent groups rm crew` to delete it entirely.\n",
}
