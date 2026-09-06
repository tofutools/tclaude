package transport

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type accessResolver struct {
	calls     int
	revoked   bool
	principal model.Principal
}

func (r *accessResolver) AuthenticateAction(context.Context, []byte) (model.Principal, error) {
	r.calls++
	if r.revoked {
		return model.Principal{}, errors.New("revoked")
	}
	return r.principal, nil
}

func TestExecutionAuthenticationRechecksAccessWithoutOperatorPromotion(t *testing.T) {
	operator, err := NewOperatorToken(testCredential)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &accessResolver{principal: model.Principal{Kind: model.PrincipalExecution, ExecutionID: "execution-a", AgentID: "agent-a"}}
	auth, err := NewCallerAuthenticator(operator, resolver)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v2/identity", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("e", 64))
	req.Header.Set("X-Agent-ID", "forged-other")
	principal, err := auth.Authenticate(req)
	if err != nil || principal.AgentID != "agent-a" || principal.ExecutionID != "execution-a" {
		t.Fatalf("resolved identity: %+v %v", principal, err)
	}
	resolver.revoked = true
	if _, err := auth.Authenticate(req); err == nil || resolver.calls != 2 {
		t.Fatal("revoked credential reused cached identity")
	}
	resolver.revoked = false
	resolver.principal = model.OperatorPrincipal()
	if _, err := auth.Authenticate(req); err == nil {
		t.Fatal("execution resolver promoted to operator")
	}
	req.Header.Set("Authorization", "Bearer "+testCredential)
	before := resolver.calls
	if principal, err := auth.Authenticate(req); err != nil || principal.Kind != model.PrincipalOperator || resolver.calls != before {
		t.Fatal("operator authentication used execution authority")
	}
}
