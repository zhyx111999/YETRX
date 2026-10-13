// Package pibridge hosts an internal, loopback-only OpenAI Chat Completions
// relay in front of the project's llm.Provider chain.
//
// The pi SDK (Node) is pointed at Relay.BaseURL with the one-time Relay.APIKey
// and a Model.ID that is decoupled from any real upstream model. The relay maps
// each request onto llm.CompletionRequest and calls the Provider registered for
// that ID, so the existing OpenAI / Responses / Anthropic adapters, retries,
// llmpool fallback and llmrec recorder all stay in the call path, and real API
// keys never leave the Go process.
//
// Semantics worth knowing:
//   - image_url parts (base64 data URL or http(s) URL) in user and tool messages
//     become llm image blocks and reach every provider format; the relay never
//     fetches a URL. Other part types (audio, file) and images in system /
//     assistant messages are rejected with 400, never silently dropped.
//   - Anthropic thinking signatures round-trip through Pi's reasoning_details
//     (see reasoning.go).
//   - tool_choice supports only "auto" and "none"; reasoning_effort only acts as
//     an off switch ("none"/"off"); max tokens is clamped to Model.MaxTokens.
//   - a stream that ends without a stop reason is an error, never a normal end.
//   - upstream failures before any output are real HTTP errors (429/4xx keep
//     their status, the rest are 502/503/504); after output has started they are
//     an SSE `data: {"error":…}` frame with no finish_reason and no [DONE].
package pibridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// Model is one model the relay exposes to Pi. ID is the name Pi sends as
// "model"; it is unrelated to the upstream model name the Provider is
// configured with.
type Model struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	ContextWindow int          `json:"contextWindow"`
	MaxTokens     int          `json:"maxTokens"`
	NonStreaming  bool         `json:"-"` // serve via Provider.Complete even for stream:true
	Provider      llm.Provider `json:"-"`
}

const (
	maxBodyBytes      = 32 << 20
	keepAliveInterval = 15 * time.Second
	closeGrace        = 10 * time.Second
)

var modelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+\-]{0,127}$`)

// modelSet is an immutable snapshot; UpdateModels swaps the whole pointer, and a
// request keeps the Provider it resolved from the snapshot it started with.
type modelSet struct {
	list []Model
	byID map[string]Model
}

func newModelSet(models []Model) (*modelSet, error) {
	set := &modelSet{list: slices.Clone(models), byID: make(map[string]Model, len(models))}
	for i, m := range set.list {
		switch {
		case !modelIDRe.MatchString(m.ID):
			return nil, fmt.Errorf("pibridge: model[%d]: invalid id %q", i, m.ID)
		case m.Provider == nil:
			return nil, fmt.Errorf("pibridge: model %q has no provider", m.ID)
		case m.ContextWindow < 0 || m.MaxTokens < 0:
			return nil, fmt.Errorf("pibridge: model %q has negative limits", m.ID)
		}
		if _, dup := set.byID[m.ID]; dup {
			return nil, fmt.Errorf("pibridge: duplicate model id %q", m.ID)
		}
		set.byID[m.ID] = m
	}
	return set, nil
}

type options struct {
	maxBody   int64
	keepAlive time.Duration
}

// Relay is a running loopback relay. Create with NewRelay; stop with Close.
type Relay struct {
	apiKey  string
	keyHash [sha256.Size]byte
	baseURL string
	created int64
	opt     options

	srv    *http.Server
	served chan struct{}
	ctx    context.Context // cancelled by Close; parent of every request context
	cancel context.CancelFunc

	models atomic.Pointer[modelSet]

	mu        sync.Mutex // guards closed + inflight.Add
	closed    bool
	inflight  sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// NewRelay validates models and starts serving on 127.0.0.1:<ephemeral>. An
// empty model list is allowed (every request then gets model_not_found).
func NewRelay(models []Model) (*Relay, error) {
	return newRelay(models, options{maxBody: maxBodyBytes, keepAlive: keepAliveInterval})
}

func newRelay(models []Model, opt options) (*Relay, error) {
	set, err := newModelSet(models)
	if err != nil {
		return nil, err
	}
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, fmt.Errorf("pibridge: generate api key: %w", err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("pibridge: listen: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rl := &Relay{
		apiKey:  "pirelay-" + hex.EncodeToString(keyBytes),
		baseURL: "http://" + ln.Addr().String() + "/v1",
		created: time.Now().Unix(),
		opt:     opt,
		served:  make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
	rl.keyHash = sha256.Sum256([]byte(rl.apiKey))
	rl.models.Store(set)
	rl.srv = &http.Server{
		Handler:           rl.track(http.HandlerFunc(rl.route)),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		defer close(rl.served)
		_ = rl.srv.Serve(ln) // always returns on Close
	}()
	return rl, nil
}

// BaseURL is the OpenAI-compatible base, e.g. http://127.0.0.1:41234/v1.
func (rl *Relay) BaseURL() string { return rl.baseURL }

// APIKey is the one-time Bearer token Node must present. It is not a real
// upstream key and is meaningless after Close.
func (rl *Relay) APIKey() string { return rl.apiKey }

// Models returns a copy of the current model list.
func (rl *Relay) Models() []Model { return slices.Clone(rl.models.Load().list) }

// UpdateModels atomically replaces the model list. On a validation error the
// previous list stays in effect. In-flight requests finish on the Provider they
// started with.
func (rl *Relay) UpdateModels(models []Model) error {
	set, err := newModelSet(models)
	if err != nil {
		return err
	}
	rl.models.Store(set)
	return nil
}

// Close stops the listener, cancels every in-flight request (and so its upstream
// call) and waits for handlers to return. It is idempotent.
func (rl *Relay) Close() error {
	rl.closeOnce.Do(func() {
		rl.mu.Lock()
		rl.closed = true
		rl.mu.Unlock()
		rl.cancel()
		rl.closeErr = rl.srv.Close()
		idle := make(chan struct{})
		go func() { rl.inflight.Wait(); close(idle) }()
		select {
		case <-idle:
		case <-time.After(closeGrace):
			rl.closeErr = errors.Join(rl.closeErr, errors.New("pibridge: in-flight requests did not stop within the close grace period"))
		}
		<-rl.served
	})
	return rl.closeErr
}

// String never exposes the API key, so accidental %v logging stays safe.
func (rl *Relay) String() string { return "pibridge.Relay(" + rl.baseURL + ")" }

func (rl *Relay) logf(format string, a ...any) { log.Printf("[pibridge] "+format, a...) }

// ---- HTTP ----

// track refuses new work once Close has started and lets Close wait for the rest.
func (rl *Relay) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rl.mu.Lock()
		if rl.closed {
			rl.mu.Unlock()
			writeAPIError(w, &apiError{status: http.StatusServiceUnavailable, typ: "server_error", code: "relay_closed", msg: "relay is closed"})
			return
		}
		rl.inflight.Add(1)
		rl.mu.Unlock()
		defer rl.inflight.Done()
		next.ServeHTTP(w, r)
	})
}

func (rl *Relay) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(h[len(prefix):])))
	return subtle.ConstantTimeCompare(got[:], rl.keyHash[:]) == 1
}

func (rl *Relay) route(w http.ResponseWriter, r *http.Request) {
	if !rl.authorized(r) {
		writeAPIError(w, &apiError{status: http.StatusUnauthorized, typ: "authentication_error", code: "invalid_api_key", msg: "missing or invalid bearer token"})
		return
	}
	var allow string
	switch r.URL.Path {
	case "/v1/chat/completions":
		if allow = http.MethodPost; r.Method == allow {
			rl.handleChat(w, r)
			return
		}
	case "/v1/models":
		if allow = http.MethodGet; r.Method == allow {
			rl.handleModels(w)
			return
		}
	default:
		writeAPIError(w, &apiError{status: http.StatusNotFound, typ: "invalid_request_error", code: "not_found", msg: "unknown path"})
		return
	}
	w.Header().Set("Allow", allow)
	writeAPIError(w, &apiError{status: http.StatusMethodNotAllowed, typ: "invalid_request_error", code: "method_not_allowed", msg: "method not allowed"})
}

func (rl *Relay) handleModels(w http.ResponseWriter) {
	type entry struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		Created       int64  `json:"created"`
		OwnedBy       string `json:"owned_by"`
		Name          string `json:"name,omitempty"`
		ContextWindow int    `json:"contextWindow,omitempty"`
		MaxTokens     int    `json:"maxTokens,omitempty"`
	}
	list := rl.models.Load().list
	data := make([]entry, 0, len(list))
	for _, m := range list {
		data = append(data, entry{m.ID, "model", rl.created, "yetrx", m.Name, m.ContextWindow, m.MaxTokens})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (rl *Relay) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rl.opt.maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeAPIError(w, &apiError{status: http.StatusRequestEntityTooLarge, typ: "invalid_request_error", code: "request_too_large",
				msg: fmt.Sprintf("request body exceeds %d bytes", tooBig.Limit)})
			return
		}
		writeAPIError(w, badRequest("read request body: %v", err))
		return
	}
	var creq chatRequest
	if err := json.Unmarshal(body, &creq); err != nil {
		writeAPIError(w, badRequest("invalid request body: %v", err))
		return
	}
	if creq.Model == "" {
		writeAPIError(w, badRequest("model is required"))
		return
	}
	m, ok := rl.models.Load().byID[creq.Model]
	if !ok {
		writeAPIError(w, &apiError{status: http.StatusNotFound, typ: "invalid_request_error", code: "model_not_found",
			msg: fmt.Sprintf("The model %q does not exist", truncateRunes(creq.Model, 64))})
		return
	}
	req, apiErr := creq.completion(m)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	if creq.Stream {
		rl.serveStream(w, r, m, req, creq.includeUsage())
	} else {
		rl.serveJSON(w, r, m, req)
	}
}

// serveJSON answers a stream:false request through Provider.Complete.
func (rl *Relay) serveJSON(w http.ResponseWriter, r *http.Request, m Model, req llm.CompletionRequest) {
	start := time.Now()
	msg, stop, usage, err := safeComplete(r.Context(), m.Provider, req)
	if err != nil {
		if r.Context().Err() != nil {
			return // client went away or relay closing
		}
		apiErr := rl.upstreamError(err)
		rl.logf("model=%s completion failed after %s (status %d): %s", m.ID, time.Since(start).Round(time.Millisecond), apiErr.status, apiErr.msg)
		writeAPIError(w, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, buildCompletion("chatcmpl-"+randomHex(12), m.ID, time.Now().Unix(), msg, stop, usage))
}

func safeComplete(ctx context.Context, p llm.Provider, req llm.CompletionRequest) (msg llm.Message, stop string, usage llm.Usage, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("provider panic: %v", rec)
		}
	}()
	return p.Complete(ctx, req)
}

type streamItem struct {
	ev  llm.StreamEvent
	err error
}

// startPump drains the provider iterator on its own goroutine so the handler can
// also select on keep-alive ticks and cancellation. A panicking Provider becomes
// an error item instead of killing the process. The goroutine ends when the
// iterator does, or as soon as ctx is cancelled (which also stops the provider).
func startPump(ctx context.Context, open func() iter.Seq2[llm.StreamEvent, error]) (<-chan streamItem, <-chan struct{}) {
	items := make(chan streamItem)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(items)
		defer func() {
			if rec := recover(); rec != nil {
				select {
				case items <- streamItem{err: fmt.Errorf("provider panic: %v", rec)}:
				case <-ctx.Done():
				}
			}
		}()
		for ev, err := range open() {
			select {
			case items <- streamItem{ev: ev, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return items, done
}

// serveStream answers a stream:true request. Response headers are committed on
// the first output or keep-alive tick, whichever is first; until then an
// upstream failure is still a proper HTTP error.
func (rl *Relay) serveStream(w http.ResponseWriter, r *http.Request, m Model, req llm.CompletionRequest, includeUsage bool) {
	ctx, cancel := context.WithCancel(r.Context())
	start := time.Now()
	items, done := startPump(ctx, func() iter.Seq2[llm.StreamEvent, error] {
		if m.NonStreaming {
			return completeEvents(ctx, m.Provider, req)
		}
		return m.Provider.Stream(ctx, req)
	})
	defer func() { cancel(); <-done }()

	enc := &streamEncoder{
		sse: newSSEWriter(w), id: "chatcmpl-" + randomHex(12), model: m.ID,
		created: time.Now().Unix(), includeUsage: includeUsage,
	}
	fail := func(err error) {
		apiErr := rl.upstreamError(err)
		rl.logf("model=%s stream failed after %s (status %d, started=%t): %s",
			m.ID, time.Since(start).Round(time.Millisecond), apiErr.status, enc.sse.committed, apiErr.msg)
		if enc.sse.committed {
			enc.fail(apiErr)
		} else {
			writeAPIError(w, apiErr)
		}
	}

	tick := time.NewTicker(rl.opt.keepAlive)
	defer tick.Stop()
	for enc.sse.err == nil {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			enc.sse.write(": keepalive\n\n")
		case it, ok := <-items:
			switch {
			case ctx.Err() != nil:
				return
			case !ok:
				if err := enc.finish(); err != nil {
					fail(err)
				}
				return
			case it.err != nil:
				fail(it.err)
				return
			default:
				enc.event(it.ev)
			}
		}
	}
}
