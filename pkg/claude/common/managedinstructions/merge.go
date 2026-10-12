package managedinstructions

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Merge gives arrived skills agent-local precedence. Receiver rules always
// remain additive: carried content never removes or grants receiver policy.
func Merge(receiver, carried []db.ManagedInstruction) ([]db.ManagedInstruction, error) {
	rules := []db.ManagedInstruction{}
	seenRules := map[string]int{}
	skills := map[string]db.ManagedInstruction{}
	for _, group := range [][]db.ManagedInstruction{receiver, carried} {
		for _, item := range group {
			if err := Validate(item.Kind, item.Name, item.Content); err != nil {
				return nil, err
			}
			if item.Kind == "rule" {
				// Content deduplication also prevents rules growing on repeated hops.
				digest := ContentDigest(item.Content)
				if index, ok := seenRules[digest]; !ok {
					seenRules[digest] = len(rules)
					rules = append(rules, item)
				} else if item.Scope == "carried" {
					rules[index] = item
				}
			} else {
				skills[item.Name] = item
			}
		}
	}
	names := []string{}
	for name := range skills {
		names = append(names, name)
	}
	sort.Strings(names)
	out := rules
	for _, name := range names {
		out = append(out, skills[name])
	}
	total := 0
	for _, item := range out {
		total += len(item.Content)
	}
	if total > db.ManagedInstructionAgentLimit {
		return nil, errors.New("effective managed instructions exceed 16 MiB")
	}
	return out, nil
}

func ContentDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
