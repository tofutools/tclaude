package cronexpr

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	_, err := Parse("*/5 * * * *")
	assert.NoError(t, err, "5-field expression")
	_, err = Parse("@daily")
	assert.NoError(t, err, "descriptor")
	_, err = Parse("@every 90s")
	assert.NoError(t, err, "@every duration")
	_, err = Parse("CRON_TZ=UTC 0 9 * * 1")
	assert.NoError(t, err, "CRON_TZ prefix")

	_, err = Parse("not a cron expr")
	assert.Error(t, err, "garbage")
	_, err = Parse("* * * * * *")
	assert.Error(t, err, "6 fields (seconds) is not standard syntax")
}

func TestNextN(t *testing.T) {
	// CRON_TZ pins evaluation so the expectations don't depend on the
	// machine's local timezone.
	base := time.Date(2026, 7, 2, 10, 2, 30, 0, time.UTC)
	got, err := NextN("CRON_TZ=UTC */5 * * * *", base, 3)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, time.Date(2026, 7, 2, 10, 5, 0, 0, time.UTC), got[0].UTC())
	assert.Equal(t, time.Date(2026, 7, 2, 10, 10, 0, 0, time.UTC), got[1].UTC())
	assert.Equal(t, time.Date(2026, 7, 2, 10, 15, 0, 0, time.UTC), got[2].UTC())
}

func TestNext_LocalWallClock_WithUTCBase(t *testing.T) {
	// The due check feeds bases parsed from RFC3339 "Z" strings (UTC
	// location). robfig matches a zone-less schedule against the base's OWN
	// location, so without Next's time.Local normalization "0 9 * * *"
	// would mean 9am UTC in the scheduler while the explainer (fed
	// time.Now(), local) predicted 9am wall clock — the regression this
	// pins. Swap time.Local for a fixed non-UTC zone so the test bites on
	// UTC CI runners too.
	prev := time.Local
	time.Local = time.FixedZone("UTC+2", 2*3600)
	t.Cleanup(func() { time.Local = prev })

	// 06:30 UTC = 08:30 on the daemon's wall clock; the next "0 9 * * *"
	// match is 09:00+02:00 — which is 07:00 UTC, not 09:00 UTC.
	base := time.Date(2026, 7, 2, 6, 30, 0, 0, time.UTC)
	next, err := Next("0 9 * * *", base)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 7, 2, 7, 0, 0, 0, time.UTC).Unix(), next.Unix(),
		"9am must be the daemon's wall clock, not the UTC base's clock")
}

func TestNext_NeverFires(t *testing.T) {
	// Feb 30 never exists; robfig's bounded search returns the zero time.
	next, err := Next("0 0 30 2 *", time.Now())
	require.NoError(t, err)
	assert.True(t, next.IsZero(), "impossible date yields zero time, got %v", next)
}

func TestDescribe(t *testing.T) {
	assert.NotEmpty(t, Describe("*/5 * * * *"), "plain expression gets a description")
	// Best-effort degradation: the describer doesn't handle @descriptors or
	// garbage — both come back as "" (never an error), because validity is
	// Parse's job.
	assert.Empty(t, Describe("@daily"))
	assert.Empty(t, Describe("not a cron expr"))
}

func TestValidate(t *testing.T) {
	assert.NoError(t, Validate("*/5 * * * *"))
	assert.NoError(t, Validate("@hourly"))
	assert.NoError(t, Validate("@every 30s"), "@every at the tick floor is fine")
	assert.Error(t, Validate("banana"), "garbage rejected")
	assert.Error(t, Validate("0 0 30 2 *"), "never-fires rejected")
	assert.Error(t, Validate("@every 5s"),
		"@every below the scheduler tick predicts fires that can't happen")
}
