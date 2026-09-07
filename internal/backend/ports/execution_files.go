package ports

import "context"

// ExecutionFileReader is a read-only composition capability. Root comes from
// the authorized execution, never from the HTTP caller.
type ExecutionFileReader interface {
	ReadExecutionFile(context.Context, ExecutionFileReadRequest) (ExecutionFileContent, error)
}
type ExecutionFileReadRequest struct{ Root, Path string }
type ExecutionFileContent struct {
	Filename string
	Content  []byte
}
