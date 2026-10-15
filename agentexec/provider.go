package agentexec

import (
	"context"
	"iter"
	"time"

	"github.com/Autumn-27/norma/llm"
)

type brokerProvider struct {
	w        *worker
	provider bool
}

func (p *brokerProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		opened, err := p.w.call(ctx, "model.open", rpcRequest{Request: req, Provider: p.provider})
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		closed := false
		defer func() {
			if !closed {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, _ = p.w.call(cleanup, "model.close", rpcRequest{ID: opened.ID})
			}
		}()
		for {
			out, err := p.w.call(ctx, "model.next", rpcRequest{ID: opened.ID})
			if !out.More {
				closed = err == nil
			}
			if err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			if !out.More {
				return
			}
			if !yield(out.Event, nil) {
				return
			}
		}
	}
}
func (p *brokerProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	out, err := p.w.call(ctx, "model.complete", rpcRequest{Request: req, Provider: p.provider})
	return out.Message, out.Stop, out.Usage, err
}
