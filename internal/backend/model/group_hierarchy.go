package model

import "fmt"

// ValidateGroupHierarchy bounds organizational nesting. It has no authority semantics.
func ValidateGroupHierarchy(groups []Group) error {
	parents := make(map[GroupID]GroupID, len(groups))
	for _, g := range groups {
		if g.ID.Validate() != nil {
			return fmt.Errorf("invalid group identity")
		}
		if _, ok := parents[g.ID]; ok {
			return fmt.Errorf("duplicate group")
		}
		parents[g.ID] = g.ParentGroupID
	}
	for id := range parents {
		seen := map[GroupID]bool{}
		current := id
		for depth := 0; current != ""; depth++ {
			if depth >= 64 || seen[current] {
				return fmt.Errorf("group hierarchy is cyclic or exceeds 64 levels")
			}
			seen[current] = true
			next, ok := parents[current]
			if !ok {
				return fmt.Errorf("group parent does not exist")
			}
			current = next
		}
	}
	return nil
}
