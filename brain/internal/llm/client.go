// Package llm talks to any OpenAI-compatible chat API (Gemini, Groq, Ollama,
// OpenRouter all offer one). Providers are tried in order: if the first one
// is rate-limited or down, the request falls through to the next.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"worldcraft/internal/metrics"
	"worldcraft/internal/ratelimit"
)

type Provider struct {
	Name       string
	BaseURL    string // must end with "/"
	APIKey     string
	Model      string
	EmbedModel string
	limiter    *ratelimit.Limiter
}

// ---- wire types (OpenAI chat completions format)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type Result struct {
	Message  Message
	Provider string
	Model    string
	Latency  time.Duration
}

// ---- client

type Client struct {
	providers  []*Provider
	embedder   *Provider
	embedLimit *ratelimit.Limiter
	http       *http.Client
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return d
}

// NewFromEnv builds the provider chain from environment variables.
// A provider is enabled only if its key is set (Ollama: OLLAMA_ENABLED=1).
func NewFromEnv() (*Client, error) {
	c := &Client{http: &http.Client{Timeout: 60 * time.Second}}
	all := map[string]*Provider{}

	if k := os.Getenv("GEMINI_API_KEY"); k != "" {
		all["gemini"] = &Provider{
			Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/", APIKey: k,
			Model:      envOr("GEMINI_MODEL", "gemini-2.5-flash-lite"),
			EmbedModel: envOr("GEMINI_EMBED_MODEL", "gemini-embedding-001"),
			limiter:    ratelimit.New(envInt("GEMINI_RPM", 12)),
		}
	}
	if k := os.Getenv("GROQ_API_KEY"); k != "" {
		all["groq"] = &Provider{
			Name: "groq", BaseURL: "https://api.groq.com/openai/v1/", APIKey: k,
			Model:   envOr("GROQ_MODEL", "llama-3.1-8b-instant"),
			limiter: ratelimit.New(envInt("GROQ_RPM", 25)),
		}
	}
	if k := os.Getenv("OPENROUTER_API_KEY"); k != "" {
		all["openrouter"] = &Provider{
			Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1/", APIKey: k,
			Model:   envOr("OPENROUTER_MODEL", "meta-llama/llama-3.3-70b-instruct:free"),
			limiter: ratelimit.New(envInt("OPENROUTER_RPM", 15)),
		}
	}
	if os.Getenv("OLLAMA_ENABLED") == "1" {
		all["ollama"] = &Provider{
			Name: "ollama", BaseURL: envOr("OLLAMA_URL", "http://localhost:11434/v1/"),
			Model:      envOr("OLLAMA_MODEL", "qwen2.5:7b"),
			EmbedModel: envOr("OLLAMA_EMBED_MODEL", "nomic-embed-text"),
			limiter:    ratelimit.New(envInt("OLLAMA_RPM", 60)),
		}
	}

	for _, name := range strings.Split(envOr("LLM_PROVIDERS", "gemini,groq,openrouter,ollama"), ",") {
		if p, ok := all[strings.TrimSpace(name)]; ok {
			c.providers = append(c.providers, p)
		}
	}
	if len(c.providers) == 0 {
		return nil, errors.New("no LLM provider configured: set GEMINI_API_KEY and/or GROQ_API_KEY in .env")
	}

	if p, ok := all[envOr("EMBED_PROVIDER", "gemini")]; ok && p.EmbedModel != "" {
		c.embedder = p
		c.embedLimit = ratelimit.New(envInt("EMBED_RPM", 60))
	}
	return c, nil
}

func (c *Client) CanEmbed() bool { return c.embedder != nil }

func (c *Client) ProviderNames() []string {
	var n []string
	for _, p := range c.providers {
		n = append(n, p.Name+"("+p.Model+")")
	}
	return n
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, e.body) }

func (c *Client) post(ctx context.Context, p *Provider, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &httpError{status: resp.StatusCode, body: msg}
	}
	return json.Unmarshal(raw, out)
}

// Chat sends one request with tools, trying each provider in order.
// Rate limits (429), server errors (5xx) and network errors move on to the next provider.
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []Tool) (*Result, error) {
	var lastErr error
	for _, p := range c.providers {
		waited, err := p.limiter.Wait(ctx)
		metrics.RateLimitWait.WithLabelValues(p.Name).Observe(waited.Seconds())
		if err != nil {
			return nil, err
		}

		start := time.Now()
		var resp chatResponse
		err = c.post(ctx, p, "chat/completions", chatRequest{
			Model: p.Model, Messages: msgs, Tools: tools, ToolChoice: "auto",
			Temperature: 0.7, MaxTokens: 400,
		}, &resp)
		latency := time.Since(start)

		status := "ok"
		if err != nil {
			status = "error"
			var he *httpError
			if errors.As(err, &he) {
				status = strconv.Itoa(he.status)
			}
		}
		metrics.LLMRequestDuration.WithLabelValues(p.Name, p.Model, status).Observe(latency.Seconds())

		if err != nil {
			lastErr = fmt.Errorf("%s: %w", p.Name, err)
			var he *httpError
			if errors.As(err, &he) && he.status < 500 && he.status != 429 {
				// A 400/401/404 is our fault (bad key, bad model name); log it loudly but still try the next provider.
				metrics.LLMFallbacks.WithLabelValues(p.Name, "client_error").Inc()
			} else {
				metrics.LLMFallbacks.WithLabelValues(p.Name, status).Inc()
			}
			continue
		}
		if len(resp.Choices) == 0 {
			lastErr = fmt.Errorf("%s: empty response", p.Name)
			metrics.LLMFallbacks.WithLabelValues(p.Name, "empty").Inc()
			continue
		}
		metrics.LLMTokens.WithLabelValues(p.Name, p.Model, "prompt").Add(float64(resp.Usage.PromptTokens))
		metrics.LLMTokens.WithLabelValues(p.Name, p.Model, "completion").Add(float64(resp.Usage.CompletionTokens))
		return &Result{Message: resp.Choices[0].Message, Provider: p.Name, Model: p.Model, Latency: latency}, nil
	}
	return nil, fmt.Errorf("all providers failed, last error: %w", lastErr)
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed turns texts into vectors in one batched call.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if c.embedder == nil {
		return nil, errors.New("no embedding provider configured")
	}
	if _, err := c.embedLimit.Wait(ctx); err != nil {
		return nil, err
	}
	var resp embedResponse
	if err := c.post(ctx, c.embedder, "embeddings", embedRequest{Model: c.embedder.EmbedModel, Input: texts}, &resp); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index >= 0 && d.Index < len(out) {
			out[d.Index] = d.Embedding
		}
	}
	for i := range out {
		if len(out[i]) == 0 {
			return nil, fmt.Errorf("embedding %d missing from response", i)
		}
	}
	return out, nil
}
