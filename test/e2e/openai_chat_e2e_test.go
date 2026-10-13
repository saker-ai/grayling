package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/saker-ai/grayling/internal/api"
	"github.com/saker-ai/grayling/internal/openaiproxy"
	"github.com/saker-ai/grayling/internal/platform"
)

func TestOpenAIChatAppliesFlagPromptAndRevocation(t *testing.T) {
	h := newAIHarness(t)
	if _, err := h.ai.RegisterResource(h.ws.ID, h.env.ID, platform.AITypePrompt, "support-tone", "Support Tone", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ai.PublishVersion(h.ws.ID, h.env.ID, platform.AITypePrompt, "support-tone", "Serve politely", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ai.PublishVersion(h.ws.ID, h.env.ID, platform.AITypePrompt, "support-tone", "Serve politely", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelAllowlist(h.ws.ID, []platform.ModelAllowEntry{
		{Provider: "qwen", Model: "qwen-max", Enabled: true},
		{Provider: "qwen", Model: "qwen-plus", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	h.publishFlag(t, aiFlag("openai-chat"), platform.RiskNormal, h.owner)
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Errorf("credential changed: %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		upstreamBody = body
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	proxy, err := openaiproxy.New(upstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.CloseIdleConnections()
	server := api.NewServer(h.store, h.api.Config, api.Options{
		ChatCompletions: proxy, OpenAIWorkspace: "acme", OpenAIEnvironment: "prod", OpenAIFlagKey: "openai-chat",
	})
	server.SetAI(h.ai)
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-model","messages":[{"role":"user","content":"Hi"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer provider-key")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec := request()
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body.String())
	}
	selected, _ := upstreamBody["model"].(string)
	if selected != "qwen-max" && selected != "qwen-plus" {
		t.Fatalf("selected model = %q", selected)
	}
	if upstreamBody["stream"] != true {
		t.Fatal("stream option was lost")
	}
	messages, _ := upstreamBody["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["content"] != "Serve politely" {
		t.Fatalf("governed messages = %#v", messages)
	}
	// Exercise the public API through the official OpenAI Go SDK. Only its
	// base URL changes; the existing provider credential and call stay intact.
	entry := httptest.NewServer(server.Handler())
	defer entry.Close()
	client := openai.NewClient(option.WithBaseURL(entry.URL+"/v1"), option.WithAPIKey("provider-key"), option.WithMaxRetries(0))
	params := openai.ChatCompletionNewParams{
		Model: "client-model", Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Hi")},
	}
	completion, err := client.Chat.Completions.New(t.Context(), params)
	if err != nil || completion == nil || len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "ok" {
		t.Fatalf("OpenAI SDK completion = %+v, err = %v", completion, err)
	}
	stream := client.Chat.Completions.NewStreaming(t.Context(), params)
	defer stream.Close()
	if !stream.Next() || len(stream.Current().Choices) != 1 || stream.Current().Choices[0].Delta.Content != "ok" {
		t.Fatalf("OpenAI SDK stream = %+v, err = %v", stream.Current(), stream.Err())
	}
	if stream.Next() || stream.Err() != nil {
		t.Fatalf("OpenAI SDK stream did not terminate cleanly: %v", stream.Err())
	}
	if err := h.ai.Revoke(h.ws.ID, h.env.ID, platform.AITypePrompt, "support-tone", "emergency", "alice"); err != nil {
		t.Fatal(err)
	}
	rec = request()
	if rec.Code != http.StatusLocked || !strings.Contains(rec.Body.String(), "resource_revoked") {
		t.Fatalf("revoked chat = %d %s", rec.Code, rec.Body.String())
	}
}
