// Package llm — минимальные обёртки над OpenAI-compatible API VseLLM:
// LLMClient (chat/completions, без стриминга — TG не стримит) и Embedder (embeddings).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Message — одно сообщение в chat-запросе.
type Message struct {
	Role    string `json:"role"`    // system | user | assistant
	Content string `json:"content"`
}

// LLMClient — chat-completions клиент. Без стриминга: TG принимает одним сообщением.
type LLMClient struct {
	baseURL   string
	apiKey    string
	model     string
	maxTokens int
	client    *http.Client
}

// NewLLMClient создаёт chat-клиент.
func NewLLMClient(baseURL, apiKey, model string, maxTokens int) *LLMClient {
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	return &LLMClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		model:     model,
		maxTokens: maxTokens,
		client:    &http.Client{Timeout: 90 * time.Second},
	}
}

// Model возвращает имя модели.
func (c *LLMClient) Model() string { return c.model }

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
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

// Chat выполняет синхронный (не стриминговый) запрос и возвращает текст ассистента.
func (c *LLMClient) Chat(ctx context.Context, messages []Message) (string, int, int, error) {
	body, err := json.Marshal(chatRequest{
		Model:     c.model,
		Messages:  messages,
		MaxTokens: c.maxTokens,
	})
	if err != nil {
		return "", 0, 0, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		safe := string(b)
		if len(safe) > 200 {
			safe = safe[:200] + "..."
		}
		return "", 0, 0, fmt.Errorf("llm HTTP %d: %s", resp.StatusCode, safe)
	}

	var r chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", 0, 0, fmt.Errorf("decode llm response: %w", err)
	}
	if len(r.Choices) == 0 {
		return "", 0, 0, fmt.Errorf("llm: empty choices")
	}
	slog.Info("llm chat done", "model", c.model,
		"in", r.Usage.PromptTokens, "out", r.Usage.CompletionTokens,
		"finish", r.Choices[0].FinishReason)
	return strings.TrimSpace(r.Choices[0].Message.Content),
		r.Usage.PromptTokens, r.Usage.CompletionTokens, nil
}
