package model

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestGroupHierarchyDepthAndMissingParent(t *testing.T) {
	groups := []Group{}
	for i := 0; i < 64; i++ {
		g := Group{ID: GroupID(fmt.Sprintf("g%d", i))}
		if i > 0 {
			g.ParentGroupID = groups[i-1].ID
		}
		groups = append(groups, g)
	}
	require.NoError(t, ValidateGroupHierarchy(groups))
	groups = append(groups, Group{ID: "extra", ParentGroupID: groups[63].ID})
	require.Error(t, ValidateGroupHierarchy(groups))
	require.Error(t, ValidateGroupHierarchy([]Group{{ID: "child", ParentGroupID: "absent"}}))
}
