package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
)

func TestRuntimeFactoryFailureDoesNotFallback(t *testing.T) {
	sentinel := errors.New("supervisor rejected process")
	SetSessionFactory(func(context.Context, agentcore.Options) (RuntimeSession, error) { return nil, sentinel })
	defer SetSessionFactory(nil)
	if _, err := NewRuntimeSession(context.Background(), agentcore.Options{}); err != sentinel {
		t.Fatalf("factory error replaced: %v", err)
	}
	if _, _, err := captureRun(context.Background(), agentcore.Options{}, "input", nil); err != sentinel {
		t.Fatalf("capture bypassed factory: %v", err)
	}
}
