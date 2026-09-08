package model

import (
	"encoding/json"
	"strconv"
)

// RetryAttempts preserves existing bounded numeric JSON and uses exact strings
// beyond JavaScript's integer range. Runtime admission applies its own ceiling.
type RetryAttempts int64

func (count RetryAttempts) MarshalJSON() ([]byte, error) {
	value := strconv.FormatInt(int64(count), 10)
	if count >= -9007199254740991 && count <= 9007199254740991 {
		return []byte(value), nil
	}
	return json.Marshal(value)
}
func (count *RetryAttempts) UnmarshalJSON(data []byte) error {
	var value ApprovalRetryAttempts
	if err := value.UnmarshalJSON(data); err != nil {
		return err
	}
	*count = RetryAttempts(value)
	return nil
}
