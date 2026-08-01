package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func costStampNS(t *testing.T, stamp string) int64 {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, stamp)
	require.NoError(t, err)
	ns, err := timeToUnixNano(parsed)
	require.NoError(t, err)
	return ns
}

// TCL-932's original defect was a chronological walk ordered by RFC3339Nano
// spelling. v181 stores the same instants as INTEGER nanoseconds, so SQLite's
// indexed ORDER BY is once again the load-bearing repair.
func TestAllCostDailyRowsOrdersIntegerInstantsBeforeReturning(t *testing.T) {
	for _, shape := range []struct {
		name, earlierText, laterText string
	}{
		{"EmptyFractionSortsLast", "2026-08-01T12:00:07Z", "2026-08-01T12:00:07.000001Z"},
		{"PrefixFractionSortsLast", "2026-08-01T12:00:07.9Z", "2026-08-01T12:00:07.95Z"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			setupTestDB(t)
			d, err := Open()
			require.NoError(t, err)
			require.Greater(t, shape.earlierText, shape.laterText,
				"control: the earlier legacy spelling sorts last and reproduces TCL-932")
			earlier, later := costStampNS(t, shape.earlierText), costStampNS(t, shape.laterText)
			require.Less(t, earlier, later, "integer representation must preserve chronology")

			_, err = d.Exec(`INSERT INTO session_cost_daily
				(session_id, day, conv_id, cost_usd, updated_at) VALUES
				('wcs-a1', '2026-08-01', 'wcsa-1111-2222-3333-4444', 1.00, ?),
				('wcs-a2', '2026-08-01', 'wcsa-1111-2222-3333-4444', 1.25, ?)`, earlier, later)
			require.NoError(t, err)

			rows, err := AllCostDailyRows()
			require.NoError(t, err)
			require.Len(t, rows, 2)
			assert.Equal(t, []string{"wcs-a1", "wcs-a2"},
				[]string{rows[0].SessionID, rows[1].SessionID},
				"the indexed INTEGER ORDER BY is the cost walk order")
			assert.Equal(t, []int64{earlier, later},
				[]int64{rows[0].UpdatedAtNS, rows[1].UpdatedAtNS})

			total := 0.0
			for _, delta := range CostDeltas(rows, false) {
				total += delta.USD
			}
			assert.InDelta(t, 1.25, total, 1e-9,
				"the conversation cost is its high-water cumulative, not the 2.25 per-session sum")
		})
	}
}

func TestAllCostDailyRowsIntegerOrderIsTotal(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	same := costStampNS(t, "2026-08-01T12:00:07.5Z")

	_, err = d.Exec(`INSERT INTO session_cost_daily
		(session_id, day, conv_id, cost_usd, updated_at) VALUES
		('wcs-a2', '2026-08-01', 'convA', 1.25, ?),
		('wcs-a1', '2026-08-01', 'convA', 1.00, ?),
		('wcs-a0', '2026-08-01', 'convA', 0.50, NULL),
		('wcs-a3', '2026-08-03', 'convA', 1.50, ?),
		('wcs-b1', '2026-08-01', 'convB', 1.00, ?),
		('s0',     '2026-08-01', '',      1.00, ?)`,
		same, same, same, same, same)
	require.NoError(t, err)

	rows, err := AllCostDailyRows()
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.SessionID)
	}
	assert.Equal(t, []string{"wcs-a0", "wcs-a1", "wcs-a2", "wcs-a3", "wcs-b1", "s0"}, got,
		"NULL first, then equal-instant session_id; conversations and days remain grouped")
	assert.Zero(t, rows[0].UpdatedAtNS, "SQL NULL crosses the internal boundary as absence")
}

// The old CompareCostStamps test included empty and unparseable spellings.
// v181 makes an unparseable runtime value impossible: malformed TEXT fails the
// migration and STRICT INTEGER rejects it thereafter. Its ordering intent is
// retained here as the integer world's sole unusable value, SQL NULL/zero.
func TestCompareCostInstants(t *testing.T) {
	earlier := costStampNS(t, "2026-08-01T12:00:07.9Z")
	later := costStampNS(t, "2026-08-01T12:00:07.95Z")
	preEpoch := costStampNS(t, "1960-01-01T00:00:00Z")
	equivalentA := costStampNS(t, "2026-08-01T12:00:07.5Z")
	equivalentB := costStampNS(t, "2026-08-01T12:00:07.500Z")
	offsetEarlier := costStampNS(t, "2026-08-01T20:00:00+09:00")
	offsetLater := costStampNS(t, "2026-08-01T05:00:00-07:00")

	for _, tc := range []struct {
		name string
		a, b int64
		want int
	}{
		{"EarlierLosesToLater", earlier, later, -1},
		{"LaterBeatsEarlier", later, earlier, 1},
		{"EqualInstantsAreEqual", earlier, earlier, 0},
		{"EquivalentLegacySpellingsCollapseToEqualIntegers", equivalentA, equivalentB, 0},
		{"DifferentOffsetsCompareAsInstants", offsetEarlier, offsetLater, -1},
		{"KnownBeatsAbsent", earlier, 0, 1},
		{"AbsentLosesToKnown", 0, earlier, -1},
		{"TwoAbsencesAreEqual", 0, 0, 0},
		{"AbsentRanksBelowPreEpochKnown", 0, preEpoch, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CompareCostInstants(tc.a, tc.b))
		})
	}
}
