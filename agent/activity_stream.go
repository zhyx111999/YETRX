package agent

import "context"

type activityStreamKey struct{}

// WithActivityStream observes text/thinking fragments before capture coalesces
// them into durable activities. It changes neither the transcript nor the ledger.
// The callback runs in the prompt loop and must not block on a client connection.
func WithActivityStream(ctx context.Context, emit func(kind, delta string)) context.Context {
	return context.WithValue(ctx, activityStreamKey{}, emit)
}

func activityStreamFrom(ctx context.Context) func(string, string) {
	emit, _ := ctx.Value(activityStreamKey{}).(func(string, string))
	return emit
}
