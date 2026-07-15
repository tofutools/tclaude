package pathv1

import "fmt"

// FoldTerminalKinds applies the authoritative closure precedence.
func FoldTerminalKinds(kinds []TerminalKind) (TerminalKind, error) {
	if len(kinds) == 0 {
		return "", fmt.Errorf("cannot fold empty terminal-kind set")
	}
	hasSkipped, hasCanceled := false, false
	for _, kind := range kinds {
		if !kind.Valid() {
			return "", fmt.Errorf("invalid terminal kind %q", kind)
		}
		switch kind {
		case TerminalFailed:
			return TerminalFailed, nil
		case TerminalSkipped:
			hasSkipped = true
		case TerminalCanceled:
			hasCanceled = true
		}
	}
	if hasSkipped {
		return TerminalSkipped, nil
	}
	if hasCanceled {
		return TerminalCanceled, nil
	}
	return TerminalImpossible, nil
}

func TerminalResult(kinds []TerminalKind) (string, error) {
	result := "completed"
	for _, kind := range kinds {
		if !kind.Valid() {
			return "", fmt.Errorf("invalid terminal kind %q", kind)
		}
		if kind == TerminalFailed || kind == TerminalSkipped {
			return "failed", nil
		}
		if kind == TerminalCanceled {
			result = "canceled"
		}
	}
	return result, nil
}
