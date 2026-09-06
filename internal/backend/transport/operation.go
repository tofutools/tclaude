package transport

import (
	"github.com/tofutools/tclaude/internal/backend/model"
	"time"
)

// Native diagnostic text can contain private endpoint or credential information.
// Public operations carry stable result codes; diagnostics stay daemon-private.
type operationView struct {
	ID          model.OperationID    `json:"id"`
	RequestID   model.RequestID      `json:"request_id"`
	Kind        model.OperationKind  `json:"kind"`
	ExecutionID model.ExecutionID    `json:"execution_id,omitempty"`
	State       model.OperationState `json:"state"`
	ResultCode  string               `json:"result_code,omitempty"`
	Revision    model.Revision       `json:"revision"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
}

func projectOperation(o model.Operation) operationView {
	return operationView{o.ID, o.RequestID, o.Kind, o.ExecutionID, o.State, o.ResultCode, o.Revision, o.CreatedAt, o.UpdatedAt}
}
