package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TCL-932. updated_at is written with time.RFC3339Nano, which trims trailing
// zeros, so the column is variable-width and a TEXT comparison is not a
// chronological one. The dashboard's cost walk requires chronological order —
// it reads a below-peak cumulative as a fresh per-session counter and restarts
// the baseline — so an inverted pair makes a resumed conversation's spend
// double-count.
//
// These tests seed the inversion DELIBERATELY. The defect reached CI as an
// intermittent macOS-only failure because it needs the clock to land on a
// particular pair of stamps; naming the stamps turns it into a failure that is
// reproducible on every platform on every run.
//
// Both shapes are covered on purpose: the empty-prefix case (a whole second,
// which is what CI actually hit) and the general prefix case (.9 before .95,
// which is far more likely and which a fix aimed only at the whole-second case
// would miss).

func costOrderRow(session, stamp string, usd float64) CostDailyRow {
	return CostDailyRow{
		SessionID: session,
		Day:       "2026-08-01",
		ConvID:    "wcsa-1111-2222-3333-4444",
		CostUSD:   usd,
		UpdatedAt: stamp,
	}
}

func TestCostDailyWalkOrderSurvivesTrimmedFractionalSeconds(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 7, 0, time.UTC)

	for _, shape := range []struct {
		name           string
		first, second  time.Duration
		wantInvertedBy string
	}{
		{
			// The shape CI hit: the first spend lands on a whole second, so
			// RFC3339Nano emits no fraction at all and 'Z' loses to '.'.
			name: "EmptyFractionSortsLast",
			// 0 and +1µs.
			first: 0, second: time.Microsecond,
			wantInvertedBy: `"…07Z" vs "…07.000001Z"`,
		},
		{
			// The general rule, and the more likely one: any stamp whose
			// fraction is a proper prefix of the later stamp's.
			name: "PrefixFractionSortsLast",
			// .9 and .95.
			first: 900 * time.Millisecond, second: 950 * time.Millisecond,
			wantInvertedBy: `"…07.9Z" vs "…07.95Z"`,
		},
	} {
		t.Run(shape.name, func(t *testing.T) {
			firstStamp := base.Add(shape.first).Format(time.RFC3339Nano)
			secondStamp := base.Add(shape.second).Format(time.RFC3339Nano)

			// The fixture only means anything if these stamps really do invert
			// under a TEXT comparison. Asserted, not assumed: if a future Go
			// release stopped trimming, this test would otherwise keep passing
			// while guarding nothing.
			require.Greater(t, firstStamp, secondStamp,
				"fixture precondition: the EARLIER stamp must sort LAST lexically (%s)",
				shape.wantInvertedBy)

			// Handed to the sort in the order SQLite's lexical ORDER BY would
			// produce — the later spend first.
			rows := []CostDailyRow{
				costOrderRow("wcs-a2", secondStamp, 1.25),
				costOrderRow("wcs-a1", firstStamp, 1.00),
			}
			sortCostDailyRowsForWalk(rows)

			assert.Equal(t, "wcs-a1", rows[0].SessionID,
				"the earlier spend must be walked first, whatever its spelling")

			total := 0.0
			for _, d := range CostDeltas(rows, false) {
				total += d.USD
			}
			// 1.25 is the conversation's high-water cumulative. 2.25 is
			// 1.00 + 1.25 — the per-session sum, which is what the walk
			// produces when it sees the resume before the session it resumed.
			assert.InDelta(t, 1.25, total, 1e-9,
				"the conversation's cost is its high-water cumulative, not the per-session sum")
		})
	}
}

// The order must be TOTAL: rows that share an instant, and rows with no usable
// stamp at all, still have exactly one correct position. Otherwise the fix
// trades a format-dependent order for a sort-dependent one.
func TestCostDailyWalkOrderIsTotal(t *testing.T) {
	same := time.Date(2026, 8, 1, 12, 0, 7, 500_000_000, time.UTC).Format(time.RFC3339Nano)

	t.Run("EqualInstantsFallBackToSessionID", func(t *testing.T) {
		rows := []CostDailyRow{
			costOrderRow("wcs-a2", same, 1.25),
			costOrderRow("wcs-a1", same, 1.00),
		}
		sortCostDailyRowsForWalk(rows)
		assert.Equal(t, []string{"wcs-a1", "wcs-a2"},
			[]string{rows[0].SessionID, rows[1].SessionID},
			"session_id is the final tiebreaker, as it has always been")
	})

	t.Run("EquivalentSpellingsOfOneInstantCompareEqual", func(t *testing.T) {
		// The same moment, written two legal ways. A lexical comparison calls
		// these different; a parsed one calls them equal and falls through to
		// session_id — which is the whole point of parsing.
		//
		// The spellings are assigned so that a LEXICAL comparison would get it
		// wrong: ".5Z" > ".500Z" ('Z' beats '0'), so lexically wcs-a2 leads and
		// this assertion fails. Assigning them the other way round would leave
		// a subtest that passes in both states, which pins nothing — checked by
		// mutation, not assumed.
		rows := []CostDailyRow{
			costOrderRow("wcs-a1", "2026-08-01T12:00:07.5Z", 1.00),
			costOrderRow("wcs-a2", "2026-08-01T12:00:07.500Z", 1.25),
		}
		sortCostDailyRowsForWalk(rows)
		assert.Equal(t, "wcs-a1", rows[0].SessionID,
			"two spellings of one instant are equal, so session_id decides")
	})

	t.Run("UnusableStampsSortFirstAndStayOrdered", func(t *testing.T) {
		// "" is the documented value for an unknown stamp, and lexically it
		// already sorted first. That position is preserved deliberately so
		// pre-updated_at history keeps the place in the walk it has always had.
		rows := []CostDailyRow{
			costOrderRow("wcs-a3", same, 1.25),
			costOrderRow("wcs-a2", "not-a-timestamp", 1.00),
			costOrderRow("wcs-a1", "", 0.50),
		}
		sortCostDailyRowsForWalk(rows)
		assert.Equal(t, []string{"wcs-a1", "wcs-a2", "wcs-a3"},
			[]string{rows[0].SessionID, rows[1].SessionID, rows[2].SessionID},
			"unusable stamps precede real ones and are ordered among themselves by session_id")
	})

	t.Run("ConversationsStayGroupedAndDaysOrdered", func(t *testing.T) {
		// The two keys the walk depends on before the timestamp ever matters.
		rows := []CostDailyRow{
			{SessionID: "s2", Day: "2026-08-02", ConvID: "convB", UpdatedAt: same},
			{SessionID: "s1", Day: "2026-08-03", ConvID: "convA", UpdatedAt: same},
			{SessionID: "s3", Day: "2026-08-01", ConvID: "convA", UpdatedAt: same},
			// No conv_id: keyed by its own session id, never merged into another
			// conversation.
			{SessionID: "s0", Day: "2026-08-01", UpdatedAt: same},
		}
		sortCostDailyRowsForWalk(rows)
		// convA, convB, then s0 — the conv-less row is keyed by its own session
		// id, and "s0" sorts after both conv ids here. The point of the case is
		// GROUPING and day order within a group, not where the fallback key
		// happens to land alphabetically.
		assert.Equal(t, []string{"s3", "s1", "s2", "s0"},
			[]string{rows[0].SessionID, rows[1].SessionID, rows[2].SessionID, rows[3].SessionID},
			"conv-key groups first and day orders within the group; the conv-less row keys on its own session id")
	})
}
