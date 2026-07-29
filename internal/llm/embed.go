package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedder — клиент /embeddings (OpenAI-compatible).
type Embedder struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewEmbedder создаёт embedder.
func NewEmbedder(baseURL, apiKey, model string) *Embedder {
	return &Embedder{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

// Model возвращает имя embedding-модели.
func (e *Embedder) Model() string { return e.model }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed возвращает вектор одного текста.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("empty text")
	}
	vecs, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, errors.New("empty embedding response")
	}
	return vecs[0], nil
}

// EmbedBatch батчит тексты в один запрос.
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	// Пустые строки могут вызывать 400 у части провайдеров — подменяем на пробел.
	cleaned := make([]string, len(texts))
	for i, t := range texts {
		if strings.TrimSpace(t) == "" {
			cleaned[i] = " "
		} else {
			cleaned[i] = t
		}
	}
	body, _ := json.Marshal(embedRequest{Model: e.model, Input: cleaned})

	// Простой retry на 429 (1с, 2с).
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost,
			e.baseURL+"/embeddings", bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		if e.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+e.apiKey)
		}
		resp, err = e.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("embeddings request: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 2 {
			resp.Body.Close()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
			continue
		}
		break
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embeddings HTTP %d: %s", resp.StatusCode, string(b))
	}

	var r embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode embeddings: %w", err)
	}
	if len(r.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: ожидалось %d, получено %d", len(texts), len(r.Data))
	}
	out := make([][]float32, len(r.Data))
	for i, d := range r.Data {
		out[i] = d.Embedding
	}
	return out, nil
}
