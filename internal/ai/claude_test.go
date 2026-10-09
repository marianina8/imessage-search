package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaude(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-test" || r.URL.Path != "/v1/messages" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["system"] != "sys" || body["model"] != "claude-test" {
			t.Errorf("body %v", body)
		}
		w.Write([]byte(`{"model":"claude-test-20261001","content":[{"type":"thinking","text":"x"},{"type":"text","text":"Hello "},{"type":"text","text":"there"}]}`))
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_MODEL", "claude-test")
	c := NewClaude()
	c.BaseURL = srv.URL
	t.Setenv("ANTHROPIC_API_KEY", "")
	if c.Ready() {
		t.Fatal("ready without key")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	text, model, err := c.Complete(context.Background(), "sys", "hi", 10)
	if err != nil || text != "Hello there" || model != "claude-test-20261001" {
		t.Fatalf("%q %q %v", text, model, err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-wrong")
	if _, _, err := c.Complete(context.Background(), "sys", "hi", 10); err == nil || !strings.Contains(err.Error(), "Anthropic API error 401") {
		t.Fatal(err)
	}
}
