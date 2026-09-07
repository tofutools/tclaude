package app

import (
	"github.com/tofutools/tclaude/internal/backend/model"
	"regexp"
)

var captureNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func validateCaptureNames(node model.WorkNode) error {
	if len(node.Captures) == 0 {
		return nil
	}
	if node.Kind != model.WorkNodeTask || len(node.Captures) > 128 {
		return fail(ErrInvalid, "up to 128 output capture names may be declared on task nodes")
	}
	seen := map[string]bool{}
	for _, name := range node.Captures {
		if len(name) > 128 || !captureNamePattern.MatchString(name) || seen[name] {
			return fail(ErrInvalid, "capture names must be unique lowercase identifiers of at most 128 bytes")
		}
		seen[name] = true
	}
	return nil
}
