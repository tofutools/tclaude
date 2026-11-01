package db

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// AuditRedacted stands in for a value the audit log must not keep.
const AuditRedacted = "[redacted]"

// auditEnvironmentName is what sandboxpolicy accepts as a variable name. A name
// outside it is not a name, and may be a value pasted into the wrong field.
var auditEnvironmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RedactAuditEnvironment replaces every environment value under v, keeping the
// names so an operator can still see what was set. It works on decoded JSON and
// edits maps in place. Any shape under an "environment" key is handled, because
// a spawn the daemon refused is audited too, and JSON decoding matches a key in
// any case. It returns how many values it replaced.
func RedactAuditEnvironment(v any) int {
	replaced := 0
	switch node := v.(type) {
	case map[string]any:
		for key, value := range node {
			if strings.EqualFold(key, "environment") {
				node[key], replaced = redactEnvironmentField(value, replaced)
				continue
			}
			replaced += RedactAuditEnvironment(value)
		}
	case []any:
		for _, value := range node {
			replaced += RedactAuditEnvironment(value)
		}
	}
	return replaced
}

func redactEnvironmentField(field any, replaced int) (any, int) {
	switch value := field.(type) {
	case nil:
		return nil, replaced
	case string:
		if value == AuditRedacted {
			return value, replaced
		}
		return AuditRedacted, replaced + 1
	case []any:
		for i, item := range value {
			value[i], replaced = redactEnvironmentEntry(item, replaced)
		}
		return value, replaced
	}
	return AuditRedacted, replaced + 1
}

func redactEnvironmentEntry(item any, replaced int) (any, int) {
	switch entry := item.(type) {
	case string:
		// A resolved launch lists bare names.
		if auditEnvironmentName.MatchString(entry) || entry == AuditRedacted {
			return entry, replaced
		}
	case map[string]any:
		if redactedAuditEntry(entry) {
			return entry, replaced
		}
		out := map[string]any{"value": AuditRedacted}
		for key, value := range entry {
			if name, ok := value.(string); ok && strings.EqualFold(key, "name") && auditEnvironmentName.MatchString(name) {
				out["name"] = name
			}
		}
		return out, replaced + 1
	}
	return AuditRedacted, replaced + 1
}

// redactedAuditEntry reports an entry this file already wrote, so a second pass
// changes nothing.
func redactedAuditEntry(entry map[string]any) bool {
	if entry["value"] != AuditRedacted {
		return false
	}
	name, named := entry["name"].(string)
	if named {
		return len(entry) == 2 && auditEnvironmentName.MatchString(name)
	}
	return len(entry) == 1
}

// redactInvalidNameError removes the name a refusal used to quote: a name that
// was not one may be a value in the wrong field.
func redactInvalidNameError(response any) bool {
	object, ok := response.(map[string]any)
	if !ok || object["code"] != "invalid_environment" {
		return false
	}
	text, _ := object["error"].(string)
	if !strings.Contains(text, `.name "`) || !strings.Contains(text, " is invalid") {
		return false
	}
	object["error"] = "environment name is invalid"
	return true
}

// redactSpawnAuditDetail redacts the environment in a stored spawn envelope. The
// fields it does not touch stay as stored. It reports false for a detail that is
// not an envelope or holds nothing to redact.
func redactSpawnAuditDetail(detail string) (string, bool) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(detail), &envelope) != nil {
		return "", false
	}
	changed := false
	for _, field := range []string{"input", "response"} {
		raw, ok := envelope[field]
		if !ok {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber() // a number stays as it was written
		var value any
		if decoder.Decode(&value) != nil {
			continue
		}
		replaced := RedactAuditEnvironment(value)
		if field == "response" && redactInvalidNameError(value) {
			replaced++
		}
		if replaced == 0 {
			continue
		}
		redacted, err := json.Marshal(value)
		if err != nil {
			return "", false
		}
		envelope[field] = redacted
		changed = true
	}
	if !changed {
		return "", false
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return "", false
	}
	return string(out), true
}
