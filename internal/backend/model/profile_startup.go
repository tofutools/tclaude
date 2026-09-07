package model

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// ValidateProfileStartup bounds reusable authoring data before it can be offered
// as launch input. Validation grants no authority and does not start any work.
func ValidateProfileStartup(startup ProfileStartup) error {
	if len(startup.AgentName) > 256 || !utf8.ValidString(startup.AgentName) || strings.ContainsAny(startup.AgentName, "\x00\r\n") || (startup.AgentName != "" && strings.TrimSpace(startup.AgentName) == "") || !utf8.ValidString(startup.Context) || !utf8.ValidString(startup.InitialMessage) || strings.ContainsRune(startup.Context, 0) || strings.ContainsRune(startup.InitialMessage, 0) {
		return fmt.Errorf("invalid profile startup text")
	}
	size := len(startup.Context) + len(startup.InitialMessage)
	if startup.Context != "" && startup.InitialMessage != "" {
		size += 2
	}
	if size > 32768 {
		return fmt.Errorf("profile context and brief exceed 32768 UTF-8 bytes")
	}
	return nil
}
