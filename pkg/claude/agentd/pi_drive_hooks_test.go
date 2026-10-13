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

// PiDriveConnectedForTest reports whether the daemon counts convID's pi drive
// channel as connected, so a test can wait for an extension's first poll.
func PiDriveConnectedForTest(convID string) bool {
	return piDrives.connected(convID)
}
