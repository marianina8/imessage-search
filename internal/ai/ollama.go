package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults used when OLLAMA_MODEL, OLLAMA_CONTEXT or OLLAMA_HOST aren't set.
const (
	DefaultOllamaModel   = "granite4.1:3b"
	DefaultOllamaContext = 8192
	DefaultOllamaHost    = "http://localhost:11434"
)

// The context window has to hold the system prompt, the messages and the reply.
// Replies go up to 4000 tokens and the report system prompt is about 2000, so that
// much is kept free and the rest is given to messages. Message text (names, dates,
// emoji) runs about 3 characters per token, which errs on the side of fitting.
const (
	ollamaReserveTokens = 6144
	ollamaCharsPerToken = 3
)

// Ollama calls a model running on this Mac through Ollama's /api/chat.
type Ollama struct {
	HTTP *http.Client
}

// NewOllama reads OLLAMA_MODEL, OLLAMA_CONTEXT and OLLAMA_HOST each time it's used,
// like NewClaude.
func NewOllama() *Ollama {
	return &Ollama{HTTP: &http.Client{Timeout: 600 * time.Second}}
}

func (o *Ollama) model() string {
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		return m
	}
	return DefaultOllamaModel
}

func (o *Ollama) host() string {
	if h := os.Getenv("OLLAMA_HOST"); h != "" {
		if !strings.Contains(h, "://") {
			h = "http://" + h
		}
		return strings.TrimRight(h, "/")
	}
	return DefaultOllamaHost
}

func (o *Ollama) contextTokens() int {
	if n, err := strconv.Atoi(os.Getenv("OLLAMA_CONTEXT")); err == nil && n > 0 {
		return n
	}
	return DefaultOllamaContext
}

func (o *Ollama) Name() string { return o.model() }

// MaxInputChars keeps the whole prompt inside num_ctx. Past that, Ollama drops the
// start of the prompt without saying so.
func (o *Ollama) MaxInputChars() int {
	ctx := o.contextTokens()
	reserve := min(ollamaReserveTokens, ctx*3/4)
	return (ctx - reserve) * ollamaCharsPerToken
}

func (o *Ollama) NotReadyHint() string {
	return fmt.Sprintf("Ollama isn't running at %s, or %s isn't downloaded. Start the Ollama app and run: ollama pull %s",
		o.host(), o.model(), o.model())
}

// Ready checks that Ollama answers and has the model downloaded.
func (o *Ollama) Ready() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]string{"model": o.model()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.host()+"/api/show", bytes.NewReader(payload))
	if err != nil {
		return false
	}
	req.Header.Set("content-type", "application/json")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (o *Ollama) Complete(ctx context.Context, system, user string, maxTokens int) (string, string, error) {
	model := o.model()
	payload, _ := json.Marshal(map[string]any{
		"model": model, "stream": false, "think": false,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"options": map[string]any{"num_ctx": o.contextTokens(), "num_predict": maxTokens},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.host()+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("Could not reach Ollama at %s: %v", o.host(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("Could not reach Ollama at %s: %v", o.host(), err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(body)
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return "", "", fmt.Errorf("Ollama error %d: %s", resp.StatusCode, msg)
	}
	var data struct {
		Model   string `json:"model"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", "", err
	}
	if data.Model != "" {
		model = data.Model
	}
	return data.Message.Content, model, nil
}
