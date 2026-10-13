package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultClaudeModel is used when ANTHROPIC_MODEL isn't set.
const DefaultClaudeModel = "claude-sonnet-5-5"

// Claude calls the Anthropic Messages API with your own API key.
type Claude struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClaude reads ANTHROPIC_API_KEY and ANTHROPIC_MODEL each time it's used,
// so a key saved from the web page takes effect right away.
func NewClaude() *Claude {
	base := os.Getenv("ANTHROPIC_BASE_URL")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return &Claude{BaseURL: base, HTTP: &http.Client{Timeout: 300 * time.Second}}
}

func (c *Claude) model() string {
	if m := os.Getenv("ANTHROPIC_MODEL"); m != "" {
		return m
	}
	return DefaultClaudeModel
}

func (c *Claude) Name() string       { return c.model() }
func (c *Claude) Ready() bool        { return os.Getenv("ANTHROPIC_API_KEY") != "" }
func (c *Claude) MaxInputChars() int { return 120_000 }
func (c *Claude) NotReadyHint() string {
	return "ANTHROPIC_API_KEY is not set. Add it to .env (see README)."
}

func (c *Claude) Complete(ctx context.Context, system, user string, maxTokens int) (string, string, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", "", errors.New("No Anthropic API key is set. Add one on the search page.")
	}
	model := c.model()
	payload, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": maxTokens, "system": system,
		"messages": []map[string]string{{"role": "user", "content": user}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("Could not reach the Anthropic API: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("Could not reach the Anthropic API: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(body)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return "", "", fmt.Errorf("Anthropic API error %d: %s", resp.StatusCode, msg)
	}
	var data struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", "", err
	}
	var text strings.Builder
	for _, b := range data.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	if data.Model != "" {
		model = data.Model
	}
	return text.String(), model, nil
}
