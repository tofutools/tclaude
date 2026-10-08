package proto

import "encoding/json"

// JobRequest selects only receiver-owned repository aliases and groups.
// No remote paths, environment, Git flags or launch profiles are accepted.
type JobRequest struct {
	ID      string `json:"id"`
	Repo    string `json:"repo"`
	Ref     string `json:"ref"`
	Group   string `json:"group"`
	Harness string `json:"harness,omitempty"`
	Command string `json:"command"`
	Timeout int64  `json:"timeout_seconds"`
	Require string `json:"require,omitempty"`
}
type JobControl struct {
	ID string `json:"id"`
}
type JobResult struct {
	Resolution string          `json:"resolution,omitempty"`
	ID         string          `json:"id"`
	State      string          `json:"state"`
	Code       string          `json:"code,omitempty"`
	Commit     string          `json:"commit,omitempty"`
	ExitCode   int             `json:"exit_code"`
	Logs       json.RawMessage `json:"logs,omitempty"`
}
