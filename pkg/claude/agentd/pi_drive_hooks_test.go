package agentd

import "time"

// SetPiDriveTimingsForTest shrinks the pi drive's poll hold, connected grace
// and waits, and returns a restore func.
func SetPiDriveTimingsForTest(poll, grace, ack, connect time.Duration) func() {
	prevPoll, prevGrace, prevAck, prevConnect := piDrivePollWait, piDriveConnectedGrace, piDriveAckTimeout, piDriveConnectTimeout
	piDrivePollWait, piDriveConnectedGrace, piDriveAckTimeout, piDriveConnectTimeout = poll, grace, ack, connect
	return func() {
		piDrivePollWait, piDriveConnectedGrace, piDriveAckTimeout, piDriveConnectTimeout = prevPoll, prevGrace, prevAck, prevConnect
	}
}
