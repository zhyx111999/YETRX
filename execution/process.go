package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limits are hard OS limits applied to the whole process tree. Zero means
// unlimited for that dimension.
//
//	MemoryBytes  total memory of the tree (Windows: job commit limit; Linux: cgroup memory.max)
//	CPUs         CPU share in logical cores, 1.0 = one core, fractions allowed
//	Processes    max live processes in the tree including the root
//	             (Linux counts tasks, i.e. threads too: leave headroom for runtime threads)
type Limits struct {
	MemoryBytes int64
	CPUs        float64
	Processes   int
}

func (l Limits) validate() error {
	if l.MemoryBytes < 0 || l.Processes < 0 || l.CPUs < 0 || math.IsNaN(l.CPUs) || math.IsInf(l.CPUs, 0) {
		return fmt.Errorf("execution: invalid limits %+v", l)
	}
	return nil
}

// stopGrace is how long a process gets to exit on its own (stdin EOF, SIGTERM)
// before the whole tree is killed.
const stopGrace = 3 * time.Second

// stderrTailBytes bounds the stderr kept for diagnostics.
const stderrTailBytes = 16 << 10

// Process is a supervised child with an RPC Peer on its stdin/stdout.
type Process struct {
	// Peer talks to the child. Calls fail with ErrClosed once the child is gone.
	Peer *Peer

	pid     int
	stderr  *tailBuffer
	closing chan struct{}
	once    sync.Once
	done    chan struct{}

	waitErr  error // valid after done
	cleanErr error // valid after done
}

// Start launches executable under limits and returns once it may receive work:
// limits are in force before the child executes its first instruction
// (Windows: created suspended, assigned to the job, then resumed; Linux: created
// directly inside its cgroup). If limits cannot be enforced Start fails with
// ErrLimitsUnavailable instead of running unconstrained.
//
// ctx bounds the child's lifetime: cancelling it stops the whole tree. env nil
// inherits the parent's environment (pass an explicit list to avoid leaking
// secrets); for non-zero limits GOMEMLIMIT/GOMAXPROCS hints are added unless
// already set — they only help Go children and are not the enforcement.
//
// The tree is stopped when ctx ends, Close is called, the Peer breaks, or the
// root exits (leftover descendants are killed). Stopping first asks (stdin EOF,
// SIGTERM), then after a short grace kills everything.
func Start(ctx context.Context, executable string, args []string, env []string, limits Limits, handler Handler) (*Process, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pipes, err := newStdio()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable, args...)
	cmd.Env = withRuntimeHints(env, limits)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pipes.inR, pipes.outW, pipes.errW
	t, err := prepareTree(cmd, limits)
	if err != nil {
		pipes.closeAll()
		return nil, err
	}

	p := &Process{
		stderr:  &tailBuffer{max: stderrTailBytes},
		closing: make(chan struct{}),
		done:    make(chan struct{}),
	}
	started := make(chan error, 1)
	go p.supervise(ctx, cmd, t, pipes, handler, started)
	if err := <-started; err != nil {
		return nil, err
	}
	return p, nil
}

// PID is the root process id.
func (p *Process) PID() int { return p.pid }

// StderrTail returns the last bytes the child wrote to stderr (bounded).
func (p *Process) StderrTail() string { return p.stderr.String() }

// Wait blocks until the root has exited, the whole tree is gone and its OS
// resources are released. It returns nil for exit status 0, the *exec.ExitError
// (wrapped) for a failed exit, or — if the supervisor had to stop the process —
// the reason: ctx.Err(), ErrClosed, or a protocol error.
func (p *Process) Wait() error {
	<-p.done
	return p.waitErr
}

// Close stops the process tree (ask, then kill after the grace period), waits
// for the real exit, and returns any cleanup failure (e.g. descendants that
// could not be killed). Idempotent.
func (p *Process) Close() error {
	p.once.Do(func() { close(p.closing) })
	<-p.done
	return p.cleanErr
}

func (p *Process) supervise(ctx context.Context, cmd *exec.Cmd, t *tree, pipes *stdio, handler Handler, started chan<- error) {
	defer close(p.done)

	err := ctx.Err()
	if err == nil {
		err = t.start(cmd)
	}
	pipes.closeChildEnds()
	if err != nil {
		pipes.closeParentEnds()
		t.close()
		started <- err
		return
	}
	p.pid = cmd.Process.Pid
	p.Peer = NewPeer(pipes.outR, pipes.inW, handler)
	stderrDone := make(chan struct{})
	go func() {
		io.Copy(p.stderr, pipes.errR)
		close(stderrDone)
	}()
	started <- nil

	exited := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		close(exited)
	}()

	var reason error
	select {
	case <-exited:
	case <-ctx.Done():
		reason = ctx.Err()
	case <-p.closing:
		reason = ErrClosed
	case <-p.Peer.Done():
		if !errors.Is(p.Peer.Err(), io.EOF) {
			reason = p.Peer.Err()
		}
	}
	if !closed(exited) {
		p.Peer.Close() // stdin EOF is the protocol-level stop signal
		t.terminate()
		select {
		case <-exited:
		case <-time.After(stopGrace):
			if reason == nil {
				reason = fmt.Errorf("execution: process kept running after its protocol stream ended: %w", p.Peer.Err())
			}
			t.kill()
			<-exited
		}
	}

	// The root is reaped; anything it left behind goes too.
	t.kill()
	cleanErr := t.drain()

	// Let the Peer consume whatever the child wrote before dying (EOF follows once
	// no process holds the pipe), then release it.
	select {
	case <-p.Peer.Done():
	case <-time.After(stopGrace):
	}
	p.Peer.Close()
	select {
	case <-stderrDone:
	case <-time.After(time.Second):
	}
	pipes.errR.Close()
	pipes.closeParentEnds()
	if cleanup := errors.Join(cleanErr, t.close()); cleanup != nil {
		p.cleanErr = &CleanupError{Err: cleanup}
	}

	switch {
	case reason != nil:
		p.waitErr = reason
	case exitErr != nil:
		p.waitErr = fmt.Errorf("execution: process exited: %w", exitErr)
	}
	p.waitErr = errors.Join(p.waitErr, p.cleanErr)
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// stdio holds both ends of the three pipes. Pipes are created here rather than
// via exec.Cmd.StdoutPipe because Cmd.Wait closes those, which could discard
// output the child wrote just before exiting.
type stdio struct {
	inR, inW   *os.File // child stdin
	outR, outW *os.File // child stdout (protocol)
	errR, errW *os.File // child stderr
}

func newStdio() (*stdio, error) {
	s := &stdio{}
	var err error
	if s.inR, s.inW, err = os.Pipe(); err == nil {
		if s.outR, s.outW, err = os.Pipe(); err == nil {
			s.errR, s.errW, err = os.Pipe()
		}
	}
	if err != nil {
		s.closeAll()
		return nil, err
	}
	return s, nil
}

func (s *stdio) closeChildEnds() {
	for _, f := range []*os.File{s.inR, s.outW, s.errW} {
		f.Close()
	}
}

func (s *stdio) closeParentEnds() {
	for _, f := range []*os.File{s.inW, s.outR, s.errR} {
		f.Close()
	}
}

func (s *stdio) closeAll() {
	for _, f := range []*os.File{s.inR, s.inW, s.outR, s.outW, s.errR, s.errW} {
		if f != nil {
			f.Close()
		}
	}
}

// withRuntimeHints adds GOMEMLIMIT/GOMAXPROCS derived from limits unless the
// caller already set them. Advisory for Go children only.
func withRuntimeHints(env []string, l Limits) []string {
	if l.MemoryBytes == 0 && l.CPUs == 0 {
		return env
	}
	out := env
	if out == nil {
		out = os.Environ()
	}
	out = append([]string(nil), out...)
	set := func(key, val string) {
		for _, kv := range out {
			if k, _, _ := strings.Cut(kv, "="); strings.EqualFold(k, key) {
				return
			}
		}
		out = append(out, key+"="+val)
	}
	if l.MemoryBytes > 0 {
		set("GOMEMLIMIT", strconv.FormatInt(l.MemoryBytes/10*8, 10))
	}
	if l.CPUs > 0 {
		set("GOMAXPROCS", strconv.Itoa(max(1, int(math.Ceil(l.CPUs)))))
	}
	return out
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	n := len(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(b) >= t.max {
		t.buf = append(t.buf[:0], b[len(b)-t.max:]...)
		return n, nil
	}
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[:copy(t.buf, t.buf[over:])]
	}
	return n, nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.buf), "?")
}
