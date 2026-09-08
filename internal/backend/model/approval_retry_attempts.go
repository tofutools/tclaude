package model

import (
	"encoding/json"
	"strconv"
)

// ApprovalRetryAttempts retains the legacy positive int64 authoring range.
// JSON output is an exact decimal string so browser editors cannot round it;
// integer JSON input remains accepted for existing authored documents.
type ApprovalRetryAttempts int64

func (count ApprovalRetryAttempts) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatInt(int64(count), 10))
}

func (count *ApprovalRetryAttempts) UnmarshalJSON(data []byte) error {
	value := string(data)
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return err
	}
	*count = ApprovalRetryAttempts(parsed)
	return nil
}
