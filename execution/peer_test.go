package execution

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func peerPair(t *testing.T, a, b Handler, lim peerLimits) (*Peer, *Peer) {
	t.Helper()
	x, y := net.Pipe()
	p, q := newPeer(x, x, a, lim), newPeer(y, y, b, lim)
	t.Cleanup(func() { p.Close(); q.Close() })
	return p, q
}

func echoHandler(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
	switch method {
	case "echo":
		return params, nil
	case "stream":
		for i := 0; i < 20; i++ {
			b, _ := json.Marshal(i)
			if err := emit(b); err != nil {
				return nil, err
			}
		}
		return json.RawMessage(`20`), nil
	case "wait":
		<-ctx.Done()
		return nil, ctx.Err()
	default:
		return nil, MethodNotFound(method)
	}
}

func exerciseRPC(t *testing.T, p *Peer) {
	t.Helper()
	ctx := testContext(t)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var got int
			if err := p.Call(ctx, "echo", i, &got); err != nil || got != i {
				t.Errorf("echo %d: %d %v", i, got, err)
			}
		}(i)
	}
	wg.Wait()
	n := 0
	var result int
	err := p.Stream(ctx, "stream", nil, func(ev json.RawMessage) error {
		var v int
		if err := json.Unmarshal(ev, &v); err != nil {
			return err
		}
		if v != n {
			t.Errorf("event %d, want %d", v, n)
		}
		n++
		return p.Call(ctx, "echo", v, nil)
	}, &result)
	if err != nil || n != 20 || result != 20 {
		t.Fatalf("stream/nested: n=%d result=%d err=%v", n, result, err)
	}
	if err := p.Call(ctx, "unknown", nil, nil); !errors.Is(err, ErrMethodNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	cancelCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := p.Call(cancelCtx, "wait", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel: %v", err)
	}
	if err := p.Call(ctx, "echo", 1, nil); err != nil {
		t.Fatalf("after cancel: %v", err)
	}
}

func TestPeerBidirectional(t *testing.T) {
	p, q := peerPair(t, echoHandler, echoHandler, peerLimits{MaxFrameBytes, MaxBacklogBytes})
	var wg sync.WaitGroup
	for _, p := range []*Peer{p, q} {
		wg.Add(1)
		go func(p *Peer) { defer wg.Done(); exerciseRPC(t, p) }(p)
	}
	wg.Wait()
}

func TestPeerFrameAndEOF(t *testing.T) {
	t.Run("local oversized preserves connection", func(t *testing.T) {
		p, _ := peerPair(t, nil, echoHandler, peerLimits{256, 8192})
		if err := p.Call(testContext(t), "echo", strings.Repeat("x", 257), nil); !errors.Is(err, ErrFrameTooLarge) {
			t.Fatal(err)
		}
		if err := p.Call(testContext(t), "echo", 1, nil); err != nil {
			t.Fatal(err)
		}
	})
	for _, kind := range []string{"oversized", "truncated", "eof"} {
		t.Run(kind, func(t *testing.T) {
			x, y := net.Pipe()
			p := NewPeer(x, x, nil)
			defer p.Close()
			defer y.Close()
			go func() {
				switch kind {
				case "oversized":
					var h [4]byte
					binary.BigEndian.PutUint32(h[:], MaxFrameBytes+1)
					y.Write(h[:])
				case "truncated":
					y.Write([]byte{0, 0, 0, 10, '{'})
				}
				y.Close()
			}()
			select {
			case <-p.Done():
			case <-testContext(t).Done():
				t.Fatal("EOF timeout")
			}
			want := io.EOF
			if kind == "oversized" {
				want = ErrFrameTooLarge
			}
			if kind == "truncated" {
				want = io.ErrUnexpectedEOF
			}
			if !errors.Is(p.Err(), want) {
				t.Fatalf("%v want %v", p.Err(), want)
			}
		})
	}
}

func TestPeerCancelBlockedWrite(t *testing.T) {
	x, y := net.Pipe()
	p := NewPeer(x, x, nil)
	defer p.Close()
	defer y.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Call(ctx, "echo", 1, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(time.Second):
		t.Fatal("blocked writer leaked")
	}
}

func TestPeerBacklog(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p, q := peerPair(t, nil, func(ctx context.Context, m string, b json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		if err := emit(json.RawMessage(`0`)); err != nil {
			return nil, err
		}
		<-entered
		for i := 0; i < 100; i++ {
			if err := emit(json.RawMessage(`1`)); err != nil {
				break
			}
		}
		close(release)
		return nil, nil
	}, peerLimits{256, 8192})
	err := p.Stream(testContext(t), "flood", nil, func(json.RawMessage) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	}, nil)
	if !errors.Is(err, ErrBacklog) {
		t.Fatalf("%v", err)
	}
	p.Close()
	q.Close()
	if n := p.backlog.Load(); n != 0 {
		t.Fatalf("retained backlog %d", n)
	}
}

func TestPeerPendingEOFAndCancelHandler(t *testing.T) {
	entered, ended := make(chan struct{}), make(chan struct{})
	p, q := peerPair(t, nil, func(ctx context.Context, _ string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		close(entered)
		<-ctx.Done()
		close(ended)
		return nil, ctx.Err()
	}, peerLimits{256, 8192})
	result := make(chan error, 1)
	go func() { result <- p.Call(testContext(t), "wait", nil, nil) }()
	<-entered
	q.Close()
	if err := <-result; !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("handler not cancelled")
	}
}

func TestPeerReplyBudgetAndControlQueue(t *testing.T) {
	p := &Peer{lim: peerLimits{256, 2048}, calls: map[uint64]*call{1: newCall(false)}}
	p.backlog.Store(2048)
	p.onReply(&frame{ID: 1, Data: json.RawMessage(`123`)})
	if _, _, done := p.calls[1].next(); !done || !errors.Is(p.calls[1].err, ErrBacklog) {
		t.Fatal("reply escaped budget")
	}
}

func TestPeerCancelReachesHandler(t *testing.T) {
	entered, ended := make(chan struct{}), make(chan struct{})
	p, _ := peerPair(t, nil, func(ctx context.Context, _ string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		close(entered)
		<-ctx.Done()
		close(ended)
		return nil, ctx.Err()
	}, peerLimits{256, 8192})
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- p.Call(ctx, "wait", nil, nil) }()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("remote handler was not cancelled")
	}
}

type shortWriter struct{ io.WriteCloser }

func (w shortWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }
func TestPeerShortWrite(t *testing.T) {
	x, y := net.Pipe()
	defer y.Close()
	p := NewPeer(x, shortWriter{x}, nil)
	defer p.Close()
	if err := p.Call(testContext(t), "echo", 1, nil); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}

func TestPeerBoundedControlQueue(t *testing.T) {
	x, y := net.Pipe()
	defer y.Close()
	p := newPeer(x, x, nil, peerLimits{256, 1})
	defer p.Close()
	// Backlog refuses each inbound call. A remote that never reads busy replies
	// exhausts the fixed control queue and is disconnected rather than retained.
	for i := uint64(1); i < 100; i++ {
		p.onCall(&frame{Type: tCall, ID: i, Method: "echo"})
	}
	select {
	case <-p.Done():
	case <-time.After(time.Second):
		t.Fatal("control overflow did not close peer")
	}
	if !errors.Is(p.Err(), ErrBacklog) {
		t.Fatal(p.Err())
	}
}
