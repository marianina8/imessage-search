// Package ai is the "brain" behind Summarize and the AI part of reports.
//
// The rest of the app only talks to the Model interface, so swapping the model
// (a different Claude model, or a model running on this Mac) means writing one
// new type that satisfies Model and returning it from FromEnv.
package ai

import (
	"context"
	"os"
)

// Model writes a reply to one prompt.
type Model interface {
	// Name is shown to the user, e.g. "claude-sonnet-5-5".
	Name() string
	// Ready reports whether the model can be used right now (for Claude: an API key is set).
	Ready() bool
	// NotReadyHint tells the user how to turn summaries on.
	NotReadyHint() string
	// MaxInputChars is how much message text one request may include. Bigger models
	// take more; small local models need much less, or they silently drop the start.
	MaxInputChars() int
	// Complete sends one system prompt and one user message and returns the reply
	// and the exact model that wrote it.
	Complete(ctx context.Context, system, user string, maxTokens int) (text, model string, err error)
}

// FromEnv picks the model from settings in the environment (or .env).
//
//	AI_PROVIDER=claude   (default) uses ANTHROPIC_API_KEY and ANTHROPIC_MODEL
func FromEnv() Model {
	switch os.Getenv("AI_PROVIDER") {
	// Add other providers here, e.g. a local model:
	// case "ollama":
	//	return NewOllama(...)
	default:
		return NewClaude()
	}
}
