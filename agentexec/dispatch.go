package agentexec

import (
	"context"
	"encoding/json"
	"fmt"
)

// Model-boundary callbacks must execute on the host Prompt consumer goroutine:
// capture's text buffer and ChatControl state belong to that goroutine. Pump
// them while awaiting child RPC, instead of running them on Peer handler threads.
type hostCallback struct {
	run    func() (json.RawMessage, error)
	result chan callbackResult
}
type callbackResult struct {
	data json.RawMessage
	err  error
}

func (s *Session) onHostThread(ctx context.Context, run func() (json.RawMessage, error)) (json.RawMessage, error) {
	job := hostCallback{run: run, result: make(chan callbackResult, 1)}
	select {
	case s.callbacks <- job:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case result := <-job.result:
		return result.data, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *Session) call(ctx context.Context, method string, params, result any) error {
	finished := make(chan error, 1)
	go func() { finished <- s.process.Peer.Call(ctx, method, params, result) }()
	for {
		select {
		case err := <-finished:
			return err
		case job := <-s.callbacks:
			job.result <- runCallback(job.run)
		}
	}
}
func runCallback(run func() (json.RawMessage, error)) (out callbackResult) {
	defer func() {
		if p := recover(); p != nil {
			out.err = fmt.Errorf("agentexec: host boundary callback panicked (%T)", p)
		}
	}()
	out.data, out.err = run()
	return out
}
