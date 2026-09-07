package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestEnvironmentBoundsRejectUnusableAuthorityAtAuthoring(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	for name, environment := range map[string]model.Environment{"reserved": {"HOME": "/alternate"}, "oversized": {"APP_VALUE": strings.Repeat("x", 16385)}, "nul": {"APP_VALUE": "a\x00b"}} {
		t.Run(name, func(t *testing.T) {
			bounds := model.ConfigurationBounds{Environments: []model.Environment{environment}}
			_, createErr := service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "group", Name: "Group", OwnerBounds: bounds})
			require.ErrorIs(t, createErr, app.ErrInvalid)
			_, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{ID: "grant", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "caller"}, Action: model.ActionLaunch, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: "target"}, Bounds: bounds}})
			require.ErrorIs(t, err, app.ErrInvalid)
			_, err = service.PutRoleAssignment(ctx, app.PutRoleAssignmentRequest{Principal: operator, Assignment: model.RoleAssignment{Bounds: bounds}})
			require.ErrorIs(t, err, app.ErrInvalid)
			_, err = service.SetGroupOwner(ctx, app.SetGroupOwnerRequest{Principal: operator, GroupID: "group", OwnerAgentID: "caller", Bounds: bounds})
			require.ErrorIs(t, err, app.ErrInvalid)
			_, err = service.RequestAccess(ctx, app.RequestAccessRequest{Context: app.RequestContext{Principal: model.Principal{Kind: model.PrincipalExecution, ExecutionID: "execution"}, RequestID: "request"}, Action: model.ActionReadStatus, Reason: "read status", Lifetime: time.Minute, Bounds: bounds})
			require.ErrorIs(t, err, app.ErrInvalid)
			_, err = service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: operator, RequestID: "rule"}, ID: "rule", Name: "Rule", Delegation: model.AutomationDelegation{Bounds: bounds}})
			require.ErrorIs(t, err, app.ErrInvalid)
		})
	}
	// The explicit empty set remains valid, as do ordinary literal values.
	require.NoError(t, (model.ConfigurationBounds{Environments: []model.Environment{nil, {"APP_VALUE": "literal $HOME\nvalue"}}}).ValidateEnvironments())
}
