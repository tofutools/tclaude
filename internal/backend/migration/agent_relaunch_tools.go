package migration

import (
	"encoding/json"
	"fmt"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
	"strings"
)

type agentRelaunchConfiguration struct {
	AutoReview        *bool                 `json:"approval_auto_review"`
	FastMode          *bool                 `json:"fast_mode"`
	ContextWindowSize *int64                `json:"context_window_size"`
	Version           int                   `json:"version"`
	Tools             *model.ToolGovernance `json:"tools"`
	Model             *string               `json:"model_id"`
	Effort            *string               `json:"effort"`
	Approval          *string               `json:"approval_policy"`
}

func decodeAgentRelaunchConfiguration(values map[string]any) (*agentRelaunchConfiguration, error) {
	raw := strings.TrimSpace(sourcev228.String(values["relaunch_profile"]))
	// Keep legacy unversioned name handling separate from the versioned resolved
	// launch record, which is independent of the birth request.
	if !strings.HasPrefix(raw, "{") {
		return nil, nil
	}
	var profile agentRelaunchConfiguration
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		return nil, err
	}
	if profile.Version != 1 {
		return nil, fmt.Errorf("unsupported relaunch profile version %d", profile.Version)
	}
	if profile.Tools != nil {
		if err := profile.Tools.Validate(); err != nil {
			return nil, err
		}
	}
	return &profile, nil
}
