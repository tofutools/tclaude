package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestRecoveryStatusVisible_RecoveredBadgeIsTransient(t *testing.T) {
	now := time.Now().UTC()
	r := db.AgentRecovery{Status: db.AgentRecoveryStatusRecovered, RecoveredAt: now}

	assert.True(t, recoveryStatusVisible(r, now.Add(-time.Second), true, now.Add(30*time.Second)))
	assert.False(t, recoveryStatusVisible(r, now.Add(time.Second), true, now.Add(2*time.Second)),
		"the first post-recovery hook clears the operational badge")
	assert.False(t, recoveryStatusVisible(r, now.Add(-time.Second), true, now.Add(time.Minute)),
		"the badge clears within one minute even without another hook")
	assert.False(t, recoveryStatusVisible(r, time.Time{}, false, now),
		"a dead successor must not be labeled recovered")
}

func TestRecoveryStatusVisible_NonRecoveredStatesRemainVisible(t *testing.T) {
	now := time.Now().UTC()
	for _, status := range []string{
		db.AgentRecoveryStatusCrashed,
		db.AgentRecoveryStatusRestarting,
		db.AgentRecoveryStatusBackoff,
		db.AgentRecoveryStatusSuppressed,
	} {
		assert.True(t, recoveryStatusVisible(db.AgentRecovery{Status: status}, now, false, now), status)
	}
	assert.False(t, recoveryStatusVisible(db.AgentRecovery{Status: db.AgentRecoveryStatusCancelled}, now, false, now))
}
