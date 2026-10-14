package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
)

func TestCaptureActivityStreamArrivesBeforeTheDurableAnswer(t *testing.T) {
	var activities []db.Activity
	var fragments []string
	provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		for _, text := range []string{"你", "好", "，世界 👋"} {
			if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: text}, nil) {
				return
			}
			if len(activities) != 0 {
				t.Fatal("a text fragment was written as a separate activity")
			}
			if fragments[len(fragments)-1] != text {
				t.Fatal("the fragment was buffered instead of observed before the next provider event")
			}
		}
		if yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil) {
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}}
	ctx := WithActivityStream(context.Background(), func(kind, delta string) {
		if kind != "text" {
			t.Fatalf("unexpected kind %s", kind)
		}
		fragments = append(fragments, delta)
	})
	text, _, err := captureRun(ctx, agentcore.Options{Provider: provider, MaxTurns: 1}, "hello", func(a db.Activity) {
		activities = append(activities, a)
	})
	if err != nil || text != "你好，世界 👋" {
		t.Fatalf("answer %q: %v", text, err)
	}
	if !reflect.DeepEqual(fragments, []string{"你", "好", "，世界 👋"}) {
		t.Fatalf("fragments: %q", fragments)
	}
	answers := 0
	for _, row := range activities {
		if row.Kind == "text" {
			t.Fatal("final text was duplicated before the result")
		}
		if row.Kind == "result" {
			answers++
			if row.Detail != text {
				t.Fatalf("durable answer: %q", row.Detail)
			}
		}
	}
	if answers != 1 {
		t.Fatalf("answers: %d", answers)
	}
}
