package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/besa-ai/besa-agent/internal/foundation/ids"
)

// HTTPTransport 是 OpenAI 兼容协议的传输实现。
type HTTPTransport struct {
	baseURL string
	apiKey  string
	client  *http.Client
	retries int
	backoff time.Duration
	jitter  float64
}

func NewHTTPTransport(baseURL, apiKey string, retries int, backoff time.Duration, jitter float64) *HTTPTransport {
	return &HTTPTransport{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client: &http.Client{
			// 不设 Client.Timeout：它会连流式响应体的读取一起计时。
			// 单次预算由每个请求的 context 控制。
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
		retries: max(0, retries),
		backoff: backoff,
		jitter:  jitter,
	}
}

func (t *HTTPTransport) Close() error {
	t.client.CloseIdleConnections()
	return nil
}

func (t *HTTPTransport) PostJSON(ctx context.Context, path string, payload any, timeout time.Duration, traceID string) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= t.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(t.backoffFor(attempt - 1)):
			}
		}
		reqCtx, cancel := withTimeout(ctx, timeout)
		resp, err := t.do(reqCtx, path, body, traceID, false)
		if err != nil {
			cancel()
			lastErr = err
			if !ClassifyNetwork(err) || ctx.Err() != nil {
				return nil, err
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			if ClassifyNetwork(readErr) && ctx.Err() == nil {
				continue
			}
			return nil, readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, t.statusError(resp.StatusCode, data)
		}
		out := map[string]any{}
		if len(bytes.TrimSpace(data)) == 0 {
			return out, nil
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, &Error{Kind: "parse", Message: "上游返回的 JSON 无法解析", Raw: truncate(string(data), 4096), Err: err}
		}
		return out, nil
	}
	return nil, lastErr
}

func (t *HTTPTransport) StreamSSE(ctx context.Context, path string, payload any, timeout time.Duration, traceID string) (SSEStream, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= t.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(t.backoffFor(attempt - 1)):
			}
		}
		reqCtx, cancel := withTimeout(ctx, timeout)
		resp, err := t.do(reqCtx, path, body, traceID, true)
		if err != nil {
			cancel()
			lastErr = err
			if ClassifyNetwork(err) && ctx.Err() == nil {
				continue
			}
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			return nil, t.statusError(resp.StatusCode, data)
		}
		return &sseStream{resp: resp, scanner: newSSEScanner(resp.Body), cancel: cancel}, nil
	}
	return nil, lastErr
}

func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func (t *HTTPTransport) do(ctx context.Context, path string, body []byte, traceID string, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	if traceID == "" {
		traceID = ids.Trace(ctx)
	}
	if traceID != "" {
		req.Header.Set("X-Trace-Id", traceID)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	return t.client.Do(req)
}

func (t *HTTPTransport) statusError(status int, data []byte) error {
	msg := string(data)
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error.Message != "" {
		msg = envelope.Error.Message
	}
	return &Error{
		Kind:       "http",
		StatusCode: status,
		Retryable:  StatusRetryable(status),
		Message:    truncate(msg, 2048),
		Raw:        truncate(string(data), 4096),
	}
}

func (t *HTTPTransport) backoffFor(attempt int) time.Duration {
	d := t.backoff
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	if d <= 0 {
		return 0
	}
	if t.jitter > 0 {
		factor := 1 + (rand.Float64()*2-1)*t.jitter
		d = time.Duration(float64(d) * factor)
	}
	return d
}

type sseStream struct {
	resp    *http.Response
	scanner *bufio.Scanner
	cancel  context.CancelFunc
	closed  bool
}

func newSSEScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	return sc
}

func (s *sseStream) Next() ([]byte, error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		return []byte(data), nil
	}
	if err := s.scanner.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &Error{Kind: "timeout", Retryable: true, Message: "读取流式响应超时", Err: err}
		}
		return nil, err
	}
	return nil, io.EOF
}

func (s *sseStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	return s.resp.Body.Close()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
