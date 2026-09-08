package app

import (
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func validateHumanChoices(human *model.HumanPerformer) error {
	if human == nil {
		return nil
	}
	if len(human.Choices) > 64 || len(human.ChoiceOutcomes) != len(human.Choices) {
		return fail(ErrInvalid, "human task choices require at most 64 exact pass/fail mappings")
	}
	for i, label := range human.Choices {
		if label == "" || strings.TrimSpace(label) != label || len(label) > 256 || !utf8.ValidString(label) || strings.ContainsRune(label, 0) {
			return fail(ErrInvalid, "human task choice requires bounded trimmed valid text")
		}
		for _, prior := range human.Choices[:i] {
			if strings.EqualFold(label, prior) {
				return fail(ErrInvalid, "human task choices must be unique ignoring case")
			}
		}
		if outcome := human.ChoiceOutcomes[label]; outcome != "pass" && outcome != "fail" {
			return fail(ErrInvalid, "each human task choice requires an exact pass or fail outcome")
		}
	}
	return nil
}
