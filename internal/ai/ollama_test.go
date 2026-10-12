package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllama(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/api/show":
			if body["model"] != "granite-test:3b" {
				w.WriteHeader(404)
				w.Write([]byte(`{"error":"model not found"}`))
			}
		case "/api/chat":
			if body["model"] != "granite-test:3b" {
				w.WriteHeader(404)
				w.Write([]byte(`{"error":"model 'other' not found"}`))
				return
			}
			opts, _ := body["options"].(map[string]any)
			msgs, _ := body["messages"].([]any)
			if body["stream"] != false || body["think"] != false || opts["num_ctx"] != 16384.0 || opts["num_predict"] != 10.0 || len(msgs) != 2 {
				t.Errorf("body %v", body)
			}
			if sys, _ := msgs[0].(map[string]any); sys["role"] != "system" || sys["content"] != "sys" {
				t.Errorf("system message %v", sys)
			}
			w.Write([]byte(`{"model":"granite-test:3b","message":{"role":"assistant","content":"Hello there"},"done":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	t.Setenv("OLLAMA_CONTEXT", "16384")
	o := NewOllama()

	t.Setenv("OLLAMA_MODEL", "missing:1b")
	if o.Ready() {
		t.Fatal("ready without the model downloaded")
	}
	if _, _, err := o.Complete(context.Background(), "sys", "hi", 10); err == nil || !strings.Contains(err.Error(), "Ollama error 404: model 'other' not found") {
		t.Fatal(err)
	}

	t.Setenv("OLLAMA_MODEL", "granite-test:3b")
	if !o.Ready() {
		t.Fatal("not ready")
	}
	text, model, err := o.Complete(context.Background(), "sys", "hi", 10)
	if err != nil || text != "Hello there" || model != "granite-test:3b" {
		t.Fatalf("%q %q %v", text, model, err)
	}

	// The messages plus the reserved system prompt and reply must fit in num_ctx.
	if got := o.MaxInputChars()/ollamaCharsPerToken + ollamaReserveTokens; got > 16384 {
		t.Fatalf("prompt budget %d tokens exceeds context", got)
	}

	srv.Close()
	if o.Ready() {
		t.Fatal("ready with Ollama not running")
	}
}

func TestOllamaDefaults(t *testing.T) {
	t.Setenv("OLLAMA_MODEL", "")
	t.Setenv("OLLAMA_CONTEXT", "")
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("AI_PROVIDER", "ollama")
	o, ok := FromEnv().(*Ollama)
	if !ok {
		t.Fatal("AI_PROVIDER=ollama did not return Ollama")
	}
	if o.Name() != "granite4.1:3b" || o.host() != "http://localhost:11434" || o.contextTokens() != 8192 {
		t.Fatalf("%q %q %d", o.Name(), o.host(), o.contextTokens())
	}
	if n := o.MaxInputChars(); n <= 0 || n/ollamaCharsPerToken+ollamaReserveTokens > 8192 {
		t.Fatalf("MaxInputChars %d doesn't fit 8192 tokens", n)
	}
}
