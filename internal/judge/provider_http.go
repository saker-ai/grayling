package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPProvider is the REAL LLM-judge integration point (docs/技术方案.md §10.4).
// It is CONFIG-ONLY in this repository: the test suite never points it at a real
// model (no API key is configured in CI). It is exercised against a local
// httptest.Server to prove the request/response plumbing, and NewHTTPProvider
// REFUSES to build without an endpoint, key and model — so the code can never
// silently pretend to have judged with a real model. Wiring a production judge
// means supplying these from configuration/secrets.
//
// The wire contract is a minimal JSON protocol a judge service (or an
// OpenAI-compatible shim) can implement:
//
//	POST {Endpoint}   Authorization: Bearer {APIKey}
//	pairwise body: {model, rubric, instruction, prompt, responseFirst, responseSecond}
//	pairwise resp: {winner: "first"|"second"|"tie", rationale}
//	single body:   {model, rubric, instruction, prompt, response}
//	single resp:   {value: number, label, rationale}

// HTTPProviderConfig carries the endpoint/credentials/model for a real judge.
type HTTPProviderConfig struct {
	Endpoint string        `json:"endpoint"`
	APIKey   string        `json:"apiKey"` // from secret/config; never hard-coded
	Model    string        `json:"model"`
	Version  string        `json:"version"`
	Timeout  time.Duration `json:"timeout"`
	// PairwiseInstruction / SingleInstruction override the default judge
	// instructions sent to the model.
	PairwiseInstruction string `json:"pairwiseInstruction,omitempty"`
	SingleInstruction   string `json:"singleInstruction,omitempty"`
}

// HTTPProvider implements Provider over HTTP.
type HTTPProvider struct {
	cfg    HTTPProviderConfig
	client *http.Client
}

// NewHTTPProvider validates the config and builds the provider. It returns an
// error (rather than a working-but-fake provider) when the endpoint, key or
// model is missing.
func NewHTTPProvider(cfg HTTPProviderConfig) (*HTTPProvider, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("judge: HTTP provider requires Endpoint (refusing to fake a real model)")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("judge: HTTP provider requires APIKey from config/secret (refusing to fake a real model)")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("judge: HTTP provider requires Model id")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &HTTPProvider{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}, nil
}

func (h *HTTPProvider) Name() string { return "http:" + h.cfg.Model }

const (
	defaultPairwiseInstruction = "You are an impartial judge. Using the rubric, compare ResponseFirst and ResponseSecond and return exactly one winner: \"first\", \"second\", or \"tie\"."
	defaultSingleInstruction   = "You are an impartial judge. Using the rubric, score the response quality as a number in [0,1] and give a short label."
)

type httpPairwiseRequest struct {
	Model          string `json:"model"`
	Rubric         string `json:"rubric"`
	Instruction    string `json:"instruction"`
	Prompt         string `json:"prompt"`
	ResponseFirst  string `json:"responseFirst"`
	ResponseSecond string `json:"responseSecond"`
}

type httpPairwiseResponse struct {
	Winner    string `json:"winner"`
	Rationale string `json:"rationale"`
}

type httpSingleRequest struct {
	Model       string `json:"model"`
	Rubric      string `json:"rubric"`
	Instruction string `json:"instruction"`
	Prompt      string `json:"prompt"`
	Response    string `json:"response"`
}

type httpSingleResponse struct {
	Value     float64 `json:"value"`
	Label     string  `json:"label"`
	Rationale string  `json:"rationale"`
}

// postJSON sends body and decodes the JSON response into out.
func (h *HTTPProvider) postJSON(ctx context.Context, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("judge: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.Endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("judge: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.cfg.APIKey)
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("judge: call endpoint: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("judge: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("judge: endpoint returned %d: %s", resp.StatusCode, string(respBody))
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("judge: decode response: %w", err)
	}
	return nil
}

// ScorePairwise calls the real endpoint. It rejects any winner value outside
// {first, second, tie} rather than passing through an unexpected verdict.
func (h *HTTPProvider) ScorePairwise(ctx context.Context, req PairwiseRequest) (PairwiseScore, error) {
	instr := h.cfg.PairwiseInstruction
	if instr == "" {
		instr = defaultPairwiseInstruction
	}
	var out httpPairwiseResponse
	err := h.postJSON(ctx, httpPairwiseRequest{
		Model: h.cfg.Model, Rubric: req.Rubric, Instruction: instr,
		Prompt: req.Prompt, ResponseFirst: req.ResponseFirst, ResponseSecond: req.ResponseSecond,
	}, &out)
	if err != nil {
		return PairwiseScore{}, err
	}
	switch out.Winner {
	case "first", "second", "tie":
	default:
		return PairwiseScore{}, fmt.Errorf("judge: endpoint returned invalid winner %q", out.Winner)
	}
	return PairwiseScore{
		Winner: out.Winner, Rationale: out.Rationale,
		Model: h.cfg.Model, Version: h.cfg.Version,
	}, nil
}

// ScoreSingle calls the real endpoint for a scalar rubric score.
func (h *HTTPProvider) ScoreSingle(ctx context.Context, req SingleRequest) (SingleScore, error) {
	instr := h.cfg.SingleInstruction
	if instr == "" {
		instr = defaultSingleInstruction
	}
	var out httpSingleResponse
	err := h.postJSON(ctx, httpSingleRequest{
		Model: h.cfg.Model, Rubric: req.Rubric, Instruction: instr,
		Prompt: req.Prompt, Response: req.Response,
	}, &out)
	if err != nil {
		return SingleScore{}, err
	}
	return SingleScore{
		Value: out.Value, Label: out.Label, Rationale: out.Rationale,
		Model: h.cfg.Model, Version: h.cfg.Version,
	}, nil
}

// Compile-time assertion that HTTPProvider satisfies Provider.
var _ Provider = (*HTTPProvider)(nil)
