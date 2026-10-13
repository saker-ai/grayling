package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"

	aisvc "github.com/saker-ai/grayling/internal/ai"
	"github.com/saker-ai/grayling/internal/openaiproxy"
	"github.com/saker-ai/grayling/pkg/model"
)

// serveGovernedChat freezes the flag decision and pinned prompt for one call.
// The original provider credential stays in Authorization for the proxy.
func (s *Server) serveGovernedChat(w http.ResponseWriter, r *http.Request) {
	key, ok := openaiproxy.BearerKey(w, r)
	if !ok {
		return
	}
	if s.ai == nil {
		openaiproxy.WriteError(w, http.StatusServiceUnavailable, "configuration_unavailable", "AI resources are unavailable")
		return
	}
	if r.ContentLength > openaiproxy.MaxRequestBytes {
		openaiproxy.WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large", "chat request exceeds 32 MiB")
		return
	}
	limitedBody := http.MaxBytesReader(w, r.Body, openaiproxy.MaxRequestBytes)
	defer limitedBody.Close()
	body, err := io.ReadAll(limitedBody)
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			openaiproxy.WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large", "chat request exceeds 32 MiB")
		} else {
			openaiproxy.WriteError(w, http.StatusBadRequest, "invalid_request", "could not read chat request")
		}
		return
	}
	var payload map[string]jsontext.Value
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		openaiproxy.WriteError(w, http.StatusBadRequest, "invalid_request", "chat request must be a JSON object")
		return
	}
	var messages []jsontext.Value
	if err := json.Unmarshal(payload["messages"], &messages); err != nil || len(messages) == 0 {
		openaiproxy.WriteError(w, http.StatusBadRequest, "invalid_request", "messages must be a nonempty array")
		return
	}
	var requestedModel string
	if err := json.Unmarshal(payload["model"], &requestedModel); err != nil || requestedModel == "" {
		openaiproxy.WriteError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	var user string
	if value, exists := payload["user"]; exists {
		if err := json.Unmarshal(value, &user); err != nil {
			openaiproxy.WriteError(w, http.StatusBadRequest, "invalid_request", "user must be a string")
			return
		}
	}
	ws, err := s.Store.GetWorkspace(s.openAIWorkspace)
	if err != nil {
		s.chatConfigurationError(w, "workspace is unavailable")
		return
	}
	env, err := s.Store.GetEnvironment(ws.ID, s.openAIEnvironment)
	if err != nil {
		s.chatConfigurationError(w, "environment is unavailable")
		return
	}
	ev, snapshot, _, err := s.evaluatorFor(r.Context(), env)
	if err != nil {
		s.chatConfigurationError(w, "published flags are unavailable")
		return
	}
	flag := snapshot.Flags[s.openAIFlagKey]
	if flag == nil || flag.Disable || flag.Type != model.FlagJSON {
		s.chatConfigurationError(w, "chat flag is missing, disabled, or not JSON")
		return
	}
	// The provider key is pseudonymized before it enters evaluation. The
	// optional OpenAI user partitions a key's rollout without storing either.
	unit := sha256.Sum256([]byte(key + "\x00" + user))
	unitID := hex.EncodeToString(unit[:])
	ctx := model.EvaluationContext{TargetingKey: unitID, Attributes: map[string]any{"user_id": unitID}}
	result := ev.Evaluate(s.openAIFlagKey, ctx)
	if result.Failed {
		s.chatConfigurationError(w, "chat flag evaluation failed")
		return
	}
	value, ok := result.Value.(map[string]any)
	if !ok {
		s.chatConfigurationError(w, "chat flag variation must be an object")
		return
	}
	modelValue, ok := value["modelRef"].(map[string]any)
	if !ok {
		s.chatConfigurationError(w, "chat flag requires modelRef")
		return
	}
	provider, _ := modelValue["provider"].(string)
	modelName, _ := modelValue["model"].(string)
	if provider == "" || modelName == "" {
		s.chatConfigurationError(w, "chat flag has invalid modelRef")
		return
	}
	if _, err := s.ai.ResolveModelRef(ws.ID, provider, modelName); err != nil {
		s.chatConfigurationError(w, "selected model is not allowed")
		return
	}
	promptValue, ok := value["promptRef"].(map[string]any)
	if !ok {
		s.chatConfigurationError(w, "chat flag requires promptRef")
		return
	}
	logicalID, _ := promptValue["logicalId"].(string)
	version, _ := promptValue["version"].(string)
	typ, _ := promptValue["type"].(string)
	if logicalID == "" || (typ != "" && typ != "prompt") {
		s.chatConfigurationError(w, "chat flag has invalid promptRef")
		return
	}
	prompt, err := s.ai.ResolveContext(r.Context(), ws.ID, env.ID, "prompt", logicalID, version)
	if err != nil {
		if errors.Is(err, aisvc.ErrRevoked) {
			openaiproxy.WriteError(w, http.StatusLocked, "resource_revoked", "selected prompt is revoked")
			return
		}
		s.chatConfigurationError(w, "selected prompt is unavailable")
		return
	}
	systemMessage, err := json.Marshal(map[string]string{"role": "system", "content": prompt.Content})
	if err != nil {
		s.chatConfigurationError(w, "selected prompt is invalid")
		return
	}
	modelJSON, err := json.Marshal(modelName)
	if err != nil {
		s.chatConfigurationError(w, "selected model is invalid")
		return
	}
	messages = append([]jsontext.Value{systemMessage}, messages...)
	messagesJSON, err := json.Marshal(messages)
	if err != nil {
		s.chatConfigurationError(w, "chat messages are invalid")
		return
	}
	payload["messages"] = messagesJSON
	payload["model"] = modelJSON
	governed, err := json.Marshal(payload)
	if err != nil {
		openaiproxy.WriteError(w, http.StatusBadRequest, "invalid_request", "chat request is invalid")
		return
	}
	if len(governed) > openaiproxy.MaxRequestBytes {
		openaiproxy.WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large", "chat request exceeds 32 MiB")
		return
	}
	// A live check closes the gap between resolving the pinned version and
	// dispatching the upstream request.
	if _, blocked := s.ai.CheckRevocation(ws.ID, env.ID, "prompt", logicalID); blocked {
		openaiproxy.WriteError(w, http.StatusLocked, "resource_revoked", "selected prompt is revoked")
		return
	}
	if result.TrackEvents && result.Experiment != nil {
		s.recordRemoteExposure(nil, env, snapshot, evalRequest{FlagKey: s.openAIFlagKey, Context: ctx}, result)
	}
	r.Body = io.NopCloser(bytes.NewReader(governed))
	r.ContentLength = int64(len(governed))
	r.TransferEncoding = nil
	r.Header.Del("Content-Length")
	s.chatCompletions.ServeHTTP(w, r)
}

func (s *Server) chatConfigurationError(w http.ResponseWriter, detail string) {
	s.logger.Printf("api: OpenAI chat configuration: %s", detail)
	openaiproxy.WriteError(w, http.StatusServiceUnavailable, "configuration_unavailable", detail)
}
