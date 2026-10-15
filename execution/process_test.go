package execution

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const helperMarker = "execution-test-helper"

func helperArgs(mode string, extra ...string) []string {
	return append([]string{"-test.run=^TestProcessHelper$", "--", helperMarker, mode}, extra...)
}

func helperEnv() []string {
	env := []string{}
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if strings.EqualFold(k, "ARTEX_CONFIG") || strings.EqualFold(k, "ARTEX_PG_DSN") {
			continue
		}
		env = append(env, v)
	}
	return append(env, "ARTEX_CONFIG="+filepath.Join(os.TempDir(), "execution-config-does-not-exist", "config"), "ARTEX_PG_DSN=")
}

// This helper never imports application packages or reads application config.
// os.Exit prevents the Go testing harness from writing PASS to protocol stdout.
func TestProcessHelper(t *testing.T) {
	idx := -1
	for i, v := range os.Args {
		if v == helperMarker {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	mode := os.Args[idx+1]
	switch mode {
	case "oversized":
		var h [4]byte
		binary.BigEndian.PutUint32(h[:], MaxFrameBytes+1)
		os.Stdout.Write(h[:])
		time.Sleep(time.Hour)
	case "eof":
		os.Exit(0)
	case "noRead":
		for {
			time.Sleep(time.Hour)
		}
	case "exit":
		os.Exit(23)
	case "leaf", "branch":
		ignoreTestSignals()
		dir := os.Args[idx+2]
		if mode == "branch" {
			cmd := exec.Command(os.Args[0], helperArgs("leaf", dir)...)
			cmd.Env = helperEnv()
			configureTestDescendant(cmd)
			if cmd.Start() != nil {
				os.Exit(24)
			}
		}
		if os.WriteFile(filepath.Join(dir, mode), []byte(strconv.Itoa(os.Getpid())), 0600) != nil {
			os.Exit(25)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	var peer *Peer
	ready := make(chan struct{})
	handler := func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		<-ready
		switch method {
		case "reverse":
			var v int
			if err := peer.Call(ctx, "echo", params, &v); err != nil {
				return nil, err
			}
			return json.Marshal(v)
		case "crash":
			os.Exit(37)
		case "stderr":
			os.Stderr.WriteString(strings.Repeat("x", stderrTailBytes*3) + "TAIL")
			return json.RawMessage(`true`), nil
		case "spawn":
			var dir string
			if err := json.Unmarshal(params, &dir); err != nil {
				return nil, err
			}
			ignoreTestSignals()
			cmd := exec.Command(os.Args[0], helperArgs("branch", dir)...)
			cmd.Env = helperEnv()
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return json.Marshal(cmd.Process.Pid)
		case "spawnAttempt":
			cmd := exec.Command(os.Args[0], helperArgs("exit")...)
			cmd.Env = helperEnv()
			err := cmd.Start()
			if err != nil {
				return json.RawMessage(`false`), nil
			}
			err = cmd.Wait()
			// A restricted job may either refuse creation or terminate the new process.
			var ee *exec.ExitError
			return json.Marshal(errors.As(err, &ee) && ee.ExitCode() == 23)
		case "nestedJob":
			child, err := Start(ctx, os.Args[0], helperArgs("rpc"), helperEnv(), Limits{Processes: 2}, nil)
			if err != nil {
				return nil, err
			}
			defer child.Close()
			return json.RawMessage(`true`), child.Peer.Call(ctx, "echo", 1, nil)
		case "memory":
			blocks := make([][]byte, 0)
			for {
				b := make([]byte, 4<<20)
				for i := 0; i < len(b); i += 4096 {
					b[i] = 1
				}
				blocks = append(blocks, b)
				if len(blocks) > 1024 {
					os.Exit(26)
				}
			}
		}
		return echoHandler(ctx, method, params, emit)
	}
	peer = NewPeer(os.Stdin, os.Stdout, handler)
	close(ready)
	<-peer.Done()
	// spawn deliberately ignores protocol EOF and termination signals.
	if mode == "stubborn" {
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(0)
}

func startHelper(t *testing.T, ctx context.Context, mode string, l Limits) *Process {
	t.Helper()
	p, err := Start(ctx, os.Args[0], helperArgs(mode), helperEnv(), l, echoHandler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return p
}

func TestProcessRPCAndCrashIsolation(t *testing.T) {
	ctx := testContext(t)
	p := startHelper(t, ctx, "rpc", Limits{})
	q := startHelper(t, ctx, "rpc", Limits{})
	reverseDone := make(chan error, 1)
	go func() {
		for i := 0; i < 24; i++ {
			var got int
			if err := p.Peer.Call(ctx, "reverse", i, &got); err != nil {
				reverseDone <- err
				return
			}
			if got != i {
				reverseDone <- fmt.Errorf("reverse got %d want %d", got, i)
				return
			}
		}
		reverseDone <- nil
	}()
	exerciseRPC(t, p.Peer)
	if err := <-reverseDone; err != nil {
		t.Fatal(err)
	}
	if err := p.Peer.Call(ctx, "echo", strings.Repeat("x", MaxFrameBytes), nil); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("actual process frame limit: %v", err)
	}
	var v int
	if err := p.Peer.Call(ctx, "reverse", 42, &v); err != nil || v != 42 {
		t.Fatalf("reverse %d %v", v, err)
	}
	if err := p.Peer.Call(ctx, "stderr", nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.HasSuffix(p.StderrTail(), "TAIL") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if tail := p.StderrTail(); len(tail) != stderrTailBytes || !strings.HasSuffix(tail, "TAIL") {
		t.Fatalf("stderr tail length %d", len(tail))
	}
	_ = p.Peer.Call(ctx, "crash", nil, nil)
	var exit *exec.ExitError
	if err := p.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 37 {
		t.Fatalf("crash exit: %v", err)
	}
	if err := q.Peer.Call(ctx, "echo", 7, &v); err != nil || v != 7 {
		t.Fatalf("other runner affected: %v", err)
	}
}

func TestProcessNonzeroExit(t *testing.T) {
	p := startHelper(t, testContext(t), "exit", Limits{})
	var exit *exec.ExitError
	if err := p.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("exit: %v", err)
	}
}

func testProcessCleanup(t *testing.T, l Limits) {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	p := startHelper(t, ctx, "stubborn", l)
	dir := t.TempDir()
	if err := p.Peer.Call(ctx, "spawn", dir, nil); err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, name := range []string{"branch", "leaf"} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err == nil {
				pid, e := strconv.Atoi(string(b))
				if e == nil {
					pids = append(pids, pid)
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never ready", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Logf("root=%d child=%d grandchild=%d", p.PID(), pids[0], pids[1])
	cancel()
	if err := p.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for _, pid := range append(pids, p.PID()) {
		deadline := time.Now().Add(time.Second)
		for testProcessAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if testProcessAlive(pid) {
			t.Errorf("process %d survived cleanup", pid)
		}
	}
}

func TestProcessCancelDescendants(t *testing.T) { testProcessCleanup(t, Limits{}) }

func TestStartInvalidAndCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Start(ctx, os.Args[0], nil, nil, Limits{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), os.Args[0], nil, nil, Limits{CPUs: -1}, nil); err == nil {
		t.Fatal("negative limit accepted")
	}
	if _, err := Start(context.Background(), filepath.Join(t.TempDir(), "absent"), nil, nil, Limits{}, nil); err == nil {
		t.Fatal("missing binary accepted")
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 8}
	for _, s := range []string{"12345", "67890", "abcdefghijk"} {
		if n, err := fmt.Fprint(b, s); err != nil || n != len(s) {
			t.Fatal(n, err)
		}
	}
	if b.String() != "defghijk" {
		t.Fatal(b.String())
	}
}

func TestProcessProtocolFailures(t *testing.T) {
	for _, mode := range []string{"oversized", "eof", "noRead"} {
		t.Run(mode, func(t *testing.T) {
			p := startHelper(t, testContext(t), mode, Limits{})
			switch mode {
			case "oversized":
				if err := p.Wait(); !errors.Is(err, ErrFrameTooLarge) {
					t.Fatalf("oversized input: %v", err)
				}
			case "eof":
				if err := p.Wait(); err != nil {
					t.Fatalf("clean EOF: %v", err)
				}
			case "noRead":
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				if err := p.Peer.Call(ctx, "echo", strings.Repeat("x", 1<<20), nil); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("blocked pipe cancellation: %v", err)
				}
				if err := p.Wait(); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("blocked pipe shutdown: %v", err)
				}
			}
		})
	}
}

func TestCleanupErrorIdentity(t *testing.T) {
	cause := errors.New("cleanup probe")
	p := &Process{done: make(chan struct{}), closing: make(chan struct{})}
	p.cleanErr = &CleanupError{Err: cause}
	p.waitErr = errors.Join(context.Canceled, p.cleanErr)
	close(p.done)
	for _, err := range []error{p.Wait(), p.Close()} {
		var cleanup *CleanupError
		if !errors.As(err, &cleanup) || !errors.Is(err, cause) {
			t.Fatalf("cleanup identity lost: %v", err)
		}
	}
	if !errors.Is(p.Wait(), context.Canceled) {
		t.Fatal("lifetime error lost")
	}
}
