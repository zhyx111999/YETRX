package resourceexec

import (
	"context"
	"github.com/Autumn-27/artex/execution"
)

// ErrMCPUnavailable identifies a server configuration, handshake or catalog
// failure after the supervised worker has started. Admission, IPC and cleanup
// failures never use this code, so callers cannot silently bypass isolation.
var ErrMCPUnavailable = &execution.Error{Code: "mcp_unavailable"}

func mcpUnavailable(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &execution.Error{Code: ErrMCPUnavailable.Code, Message: err.Error()}
}
