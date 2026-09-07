package model

import "time"

// TerminalFile is a historical upload receipt. NativePath names provider-owned
// user content, not a credential or a promise that the file still exists.
type TerminalFile struct {
	OperationID OperationID
	ExecutionID ExecutionID
	Filename    string
	Size        int64
	SHA256      string
	NativePath  string    `json:",omitempty"`
	StagedAt    time.Time `json:",omitempty"`
}
