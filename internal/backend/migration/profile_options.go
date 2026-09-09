package migration

import (
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
	"strings"
)

// V1 spawn profiles contain optional launch fields, not a working directory.
// Preserve omissions and nullable toggles rather than resolving ambient defaults
// during an offline import. A source record with a complete explicit launch
// configuration retains its existing representation.
func importedProfileOptions(values map[string]any, desired model.DesiredConfiguration) *model.ConfigurationOptions {
	if desired.Harness != "" && desired.WorkingDirectory != "" && desired.Approval != "" && desired.Sandbox != "" {
		return nil
	}
	text := func(value string) *string {
		if value == "" {
			return nil
		}
		return &value
	}
	out := &model.ConfigurationOptions{Harness: text(desired.Harness), Model: text(desired.Model), Effort: text(desired.Effort), WorkingDirectory: text(desired.WorkingDirectory), Environment: desired.Environment}
	if values["peer_messaging"] != nil {
		value, _ := importedPeerMessaging(values["peer_messaging"])
		out.PeerMessaging = &value
	}
	if values["auto_memory"] != nil {
		value, _ := importedAutoMemory(values["auto_memory"])
		out.AutoMemory = &value
	}
	if values["auto_review"] != nil {
		value, _ := importedAutoReview(values["auto_review"])
		out.AutoReview = &value
	}
	if values["fast_mode"] != nil {
		value := importedFastMode(values["fast_mode"])
		out.FastMode = &value
	}
	if raw := sourcev228.String(values["tools"]); raw != "" {
		value := model.ToolGovernance(raw)
		out.ToolGovernance = &value
	}
	if raw := sourcev228.String(values["approval"]); raw != "" {
		value := desired.Approval
		if value == "" {
			// Approval remains provider-specific intent. The final selected
			// harness determines whether an inherited tier is applicable.
			canonical := strings.ReplaceAll(strings.ToLower(raw), "_", "-")
			switch canonical {
			case "acceptedits":
				canonical = "acceptEdits"
			case "dontask":
				canonical = "dontAsk"
			case "bypasspermissions":
				canonical = "bypassPermissions"
			}
			value = model.ApprovalMode(canonical)
		}
		out.Approval = &value
	}
	if raw := sourcev228.String(values["sandbox"]); raw != "" {
		value := model.SandboxMode(strings.ReplaceAll(strings.ToLower(raw), "-", "_"))
		out.Sandbox = &value
	}
	return out
}
