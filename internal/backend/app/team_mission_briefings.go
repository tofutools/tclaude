package app

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

var missionPlaceholder = strings.NewReplacer("{{task}}", "", "{{mission}}", "")

func resolveTeamMissionBriefings(team model.TeamDefinition, mission string, initial bool) (model.TeamDefinition, error) {
	templated := false
	for _, brief := range team.Briefings {
		if brief.Syntax != "" {
			templated = true
		}
	}
	if !templated {
		return team, nil
	}
	if !utf8.ValidString(mission) || strings.ContainsRune(mission, 0) {
		return model.TeamDefinition{}, fail(ErrInvalid, "team mission must be valid text")
	}
	team.Briefings = slices.Clone(team.Briefings)
	for i := range team.Briefings {
		brief := &team.Briefings[i]
		if brief.Syntax == "" {
			continue
		}
		if brief.Syntax != "mission-v1" {
			return model.TeamDefinition{}, fail(ErrInvalid, "unsupported team briefing syntax")
		}
		limit := maxMessageBodyBytes
		if brief.Timing == model.BriefingBeforeFirstWork {
			limit = 32768
		}
		count := strings.Count(brief.Body, "{{task}}") + strings.Count(brief.Body, "{{mission}}")
		base := len(missionPlaceholder.Replace(brief.Body))
		if base > limit || (count > 0 && len(mission) > (limit-base)/count) {
			return model.TeamDefinition{}, fail(ErrInvalid, "expanded team briefing exceeds input limit")
		}
		brief.Body = strings.NewReplacer("{{task}}", mission, "{{mission}}", mission).Replace(brief.Body)
		if strings.TrimSpace(brief.Body) == "" || !utf8.ValidString(brief.Body) || strings.ContainsRune(brief.Body, 0) {
			return model.TeamDefinition{}, fail(ErrInvalid, "expanded team briefing requires valid nonempty text")
		}
		// This detached copy is resolved input, never an authored revision.
		brief.Syntax = ""
	}
	if initial {
		for _, member := range team.Members {
			text := strings.TrimSpace(mission)
			if len(text) > 32768 {
				return model.TeamDefinition{}, fail(ErrInvalid, "combined initial team briefing exceeds 32768 bytes")
			}
			for _, brief := range team.Briefings {
				if brief.Timing != model.BriefingBeforeFirstWork || !slices.Contains(teamBriefRecipients(team, brief), member.Key) {
					continue
				}
				body := strings.TrimRightFunc(brief.Body, unicode.IsSpace)
				if text == "" {
					body = strings.TrimSpace(body)
				}
				if body == "" {
					continue
				}
				if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
					return model.TeamDefinition{}, fail(ErrInvalid, "initial team briefing requires valid text")
				}
				separator := ""
				if text != "" {
					separator = "\n\n"
				}
				if len(text)+len(separator)+len(body) > 32768 {
					return model.TeamDefinition{}, fail(ErrInvalid, "combined initial team briefing exceeds 32768 bytes")
				}
				text += separator + body
			}
		}
	}
	return team, nil
}
