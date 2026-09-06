package transport

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// CallerAuthenticator accepts the provisioned operator credential or resolves
// an execution action credential against live application access state. It does
// not cache either authority decisions or execution credential generations.
type CallerAuthenticator struct {
	operator   *OperatorToken
	executions app.ActionAuthenticator
}

func NewCallerAuthenticator(operator *OperatorToken, executions app.ActionAuthenticator) (*CallerAuthenticator, error) {
	if operator == nil || executions == nil {
		return nil, errors.New("operator and execution authenticators are required")
	}
	return &CallerAuthenticator{operator: operator, executions: executions}, nil
}

func (a *CallerAuthenticator) Authenticate(r *http.Request) (model.Principal, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return model.Principal{}, ErrUnauthenticated
	}
	credential := strings.TrimPrefix(values[0], "Bearer ")
	if len(credential) < 32 || len(credential) > 4096 || strings.ContainsAny(credential, " \t\r\n") {
		return model.Principal{}, ErrUnauthenticated
	}
	if principal, err := a.operator.Authenticate(r); err == nil {
		return principal, nil
	}
	principal, err := a.executions.AuthenticateAction(r.Context(), []byte(credential))
	if err != nil {
		return model.Principal{}, ErrUnauthenticated
	}
	// Execution authentication cannot be used as an alternative operator issuer.
	if principal.Kind != model.PrincipalExecution {
		return model.Principal{}, ErrUnauthenticated
	}
	return principal, nil
}
