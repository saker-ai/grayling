package openaiproxy_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/openaiproxy"
)

func TestChatCompletionsPassThrough(t *testing.T) {
	t.Parallel()
	const requestBody = `{"model":"example","messages":[{"role":"user","content":"hello"}],"stream":false}`
	const responseBody = `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/vendor/v1/chat/completions" || r.URL.RawQuery != "test=1" {
			t.Errorf("upstream request: %s %s", r.Method, r.URL)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Errorf("console cookie reached upstream: %q", got)
		}
		if got := r.Header.Get("OpenAI-Organization"); got != "org-1" {
			t.Errorf("OpenAI-Organization = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != requestBody {
			t.Errorf("body = %q, err = %v", body, err)
		}
		w.Header().Set("X-Upstream", "yes")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()
	handler, err := openaiproxy.New(upstream.URL + "/vendor/v1")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?test=1", strings.NewReader(requestBody))
	req.Header.Set("Authorization", "Bearer upstream-key")
	req.Header.Set("Cookie", "golab_session=private")
	req.Header.Set("OpenAI-Organization", "org-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || rec.Body.String() != responseBody || rec.Header().Get("X-Upstream") != "yes" {
		t.Fatalf("response = %d %q, headers = %v", rec.Code, rec.Body.String(), rec.Header())
	}
}

func TestChatCompletionsStreamsBeforeUpstreamFinishes(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	handler, err := openaiproxy.New(upstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer upstream-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, `"first"`) {
		t.Fatalf("first stream chunk = %q, err = %v", line, err)
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(rest), "data: [DONE]") {
		t.Fatalf("stream tail = %q, err = %v", rest, err)
	}
}

func TestChatCompletionsRejectsMissingBearer(t *testing.T) {
	t.Parallel()
	called := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called <- struct{}{} }))
	defer upstream.Close()
	handler, err := openaiproxy.New(upstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, authorization string
		status              int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"golab credential", "Bearer glab_admin_example", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set("Authorization", tc.authorization)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
	select {
	case <-called:
		t.Fatal("upstream was called for a rejected credential")
	default:
	}
}

func TestChatCompletionsKeepsUpstreamError(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream rejected key"}}`)
	}))
	defer upstream.Close()
	handler, err := openaiproxy.New(upstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer bad-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Body.String() != `{"error":{"message":"upstream rejected key"}}` {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestChatCompletionsRejectsOversizedChunkedBody(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	handler, err := openaiproxy.New(upstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(bytes.Repeat([]byte("x"), (32<<20)+1)))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer upstream-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized chunked body status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func TestNewRejectsInvalidUpstream(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "ftp://example.com/v1", "https://example.com", "https://user:pass@example.com/v1", "https://example.com/v1?key=secret"} {
		if _, err := openaiproxy.New(raw); err == nil {
			t.Errorf("accepted invalid upstream %q", raw)
		}
	}
}
