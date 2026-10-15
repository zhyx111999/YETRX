package resourceexec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

func (c *mcpConnection) openHTTP() error {
	if c.config.URL == "" {
		return errors.New("resourceexec: MCP URL missing")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if c.config.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	} // explicit catalog option
	hc := &http.Client{Transport: tr}
	var mu sync.Mutex
	sid := ""
	endpoint := c.config.URL
	version := "2025-06-18"
	if c.config.Transport == "sse" {
		version = "2024-11-05"
	}
	makeRequest := func(ctx context.Context, method, target string, body io.Reader) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, method, target, body)
		if err != nil {
			return nil, errors.New("resourceexec: invalid MCP HTTP request")
		}
		for k, v := range c.config.Env {
			req.Header.Set(k, v)
		}
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", version)
		mu.Lock()
		session := sid
		mu.Unlock()
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		return req, nil
	}
	var stream io.ReadCloser
	c.shutdown = func() error {
		if stream != nil {
			_ = stream.Close()
		}
		mu.Lock()
		session := sid
		mu.Unlock()
		if session != "" && c.config.Transport == "http" {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if req, err := makeRequest(ctx, http.MethodDelete, c.config.URL, nil); err == nil {
				if resp, err := hc.Do(req); err == nil {
					resp.Body.Close()
				}
			}
		}
		tr.CloseIdleConnections()
		return nil
	}
	if c.config.Transport == "sse" {
		req, err := makeRequest(c.ctx, http.MethodGet, c.config.URL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, err := hc.Do(req)
		if err != nil {
			return errors.New("resourceexec: MCP SSE connect failed")
		}
		stream = resp.Body
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("resourceexec: MCP SSE status %d", resp.StatusCode)
		}
		ready := make(chan string, 1)
		go func() {
			_ = readEvents(stream, func(event, data string) bool {
				if event == "endpoint" {
					select {
					case ready <- data:
					default:
					}
					return true
				}
				var f mcpFrame
				if json.Unmarshal([]byte(data), &f) == nil {
					c.deliver(f)
				}
				return true
			})
			c.cancel()
		}()
		select {
		case next := <-ready:
			base, err := url.Parse(c.config.URL)
			if err != nil {
				return err
			}
			u, err := url.Parse(next)
			if err != nil {
				return errors.New("resourceexec: invalid SSE endpoint")
			}
			target := base.ResolveReference(u)
			// Never forward authorization headers to an unrelated endpoint origin.
			if target.Scheme != base.Scheme || target.Host != base.Host {
				return errors.New("resourceexec: cross-origin SSE endpoint")
			}
			endpoint = target.String()
		case <-c.ctx.Done():
			return errors.New("resourceexec: MCP SSE endpoint unavailable")
		}
	}
	c.send = func(ctx context.Context, f mcpFrame) error {
		data, err := json.Marshal(f)
		if err != nil {
			return err
		}
		req, err := makeRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			return err
		}
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("resourceexec: MCP HTTP request failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("resourceexec: MCP HTTP status %d", resp.StatusCode)
		}
		if session := resp.Header.Get("Mcp-Session-Id"); session != "" {
			mu.Lock()
			sid = session
			mu.Unlock()
		}
		if f.ID == 0 || c.config.Transport == "sse" {
			return nil
		}
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			matched := false
			err = readEvents(resp.Body, func(_ string, data string) bool {
				var response mcpFrame
				if json.Unmarshal([]byte(data), &response) == nil {
					c.deliver(response)
					matched = response.ID == f.ID && response.Method == ""
				}
				return !matched
			})
			if err != nil {
				return err
			}
			if !matched {
				return errors.New("resourceexec: MCP event stream ended without response")
			}
			return nil
		}
		var response mcpFrame
		if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&response); err != nil {
			return errors.New("resourceexec: malformed MCP JSON response")
		}
		if response.ID != f.ID {
			return errors.New("resourceexec: MCP response ID mismatch")
		}
		c.deliver(response)
		return nil
	}
	return nil
}

// readEvents handles multiline SSE data and bounds both lines and whole events.
func readEvents(r io.Reader, visit func(string, string) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	event := ""
	var data strings.Builder
	flush := func() bool {
		if data.Len() == 0 {
			event = ""
			return true
		}
		s := strings.TrimSuffix(data.String(), "\n")
		more := visit(event, s)
		event = ""
		data.Reset()
		return more
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if !flush() {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(line[6:])
		}
		if strings.HasPrefix(line, "data:") {
			v := strings.TrimPrefix(line[5:], " ")
			if data.Len()+len(v) > 8<<20 {
				return errors.New("resourceexec: MCP event too large")
			}
			data.WriteString(v)
			data.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	flush()
	return nil
}
