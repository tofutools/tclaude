package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AskUserQuestionTimeout retains an explicit native-inherit choice separately
// from an omitted profile field. Only final provider emission omits inherit.
type AskUserQuestionTimeout string

func ParseAskUserQuestionTimeout(raw string) (AskUserQuestionTimeout, error) {
	value := strings.TrimSpace(raw)
	switch value {
	case "", "inherit", "never", "60s", "5m", "10m":
		return AskUserQuestionTimeout(value), nil
	default:
		return "", fmt.Errorf("question timeout requires inherit, never, 60s, 5m or 10m")
	}
}

func (v *AskUserQuestionTimeout) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	parsed, err := ParseAskUserQuestionTimeout(value)
	if err == nil {
		*v = parsed
	}
	return err
}

func (v AskUserQuestionTimeout) Validate(harness string) error {
	parsed, err := ParseAskUserQuestionTimeout(string(v))
	if err != nil {
		return err
	}
	if parsed != v {
		return fmt.Errorf("question timeout must be normalized")
	}
	if v != "" && harness != "claude" {
		return fmt.Errorf("question timeout is available only for Claude Code")
	}
	return nil
}
