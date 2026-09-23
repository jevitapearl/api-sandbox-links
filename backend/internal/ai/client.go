// Package ai wraps the Gemini API (called server-side from Go) behind two
// tightly-scoped functions, per the locked architecture decision: (a) suggest
// likely-required env vars from repo files, and (b) draft a basic
// OpenAPI/Swagger skeleton from detected route files. Nothing here tries to be
// autonomous; every suggestion is surfaced to the user for editing before use.
package ai

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/genai"
)

// Client is a thin, gracefully-degrading wrapper around the genai SDK. When no
// API key is configured, the Client is "disabled" and callers decide how to
// fall back (the platform still deploys fine without AI).
type Client struct {
	gen   *genai.Client
	model string
	log   *slog.Logger
}

// New builds an AI client from an API key. An empty key returns a no-op
// client (Enabled() == false) rather than an error, so the platform boots
// without Gemini configured.
func New(ctx context.Context, apiKey, model string, logger *slog.Logger) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		logger.Info("ai: GEMINI_API_KEY not set — AI features disabled")
		return &Client{log: logger}, nil
	}
	if model == "" {
		// genai examples use the flash model for cheap, fast completions.
		model = "gemini-2.5-flash"
	}
	c, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("ai: creating gemini client: %w", err)
	}
	return &Client{gen: c, model: model, log: logger}, nil
}

// Enabled reports whether a live backend is configured.
func (c *Client) Enabled() bool { return c.gen != nil }

// complete runs one prompt and returns the model's raw text reply.
func (c *Client) complete(ctx context.Context, system, user string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("ai: not enabled (GEMINI_API_KEY unset)")
	}
	full := system + "\n\n--- USER CONTEXT ---\n" + user
	resp, err := c.gen.Models.GenerateContent(ctx, c.model, genai.Text(full), nil)
	if err != nil {
		return "", fmt.Errorf("ai: generating content: %w", err)
	}
	text := "No response"
	if t := resp.Text(); t != "" {
		text = t
	}
	return text, nil
}

// SuggestEnvVars asks Gemini to list the environment variables a repo most
// likely needs, given a handful of its most informative files. Files is a
// map of path -> content to keep the prompt small.
func (c *Client) SuggestEnvVars(ctx context.Context, repoName string, files map[string]string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s\n", repoName)
	for path, content := range files {
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", path, content)
		// Guard against gigantic files blowing the context window.
		if b.Len() > 12000 {
			break
		}
	}
	return c.complete(ctx, SystemEnvVarsPrompt, b.String())
}

// GenerateOpenAPI asks Gemini to turn detected route snippets into a basic
// OpenAPI 3.0 skeleton (YAML). routes is a compact summary of endpoints.
func (c *Client) GenerateOpenAPI(ctx context.Context, repoName string, routes string) (string, error) {
	prompt := fmt.Sprintf("Repository: %s\n\nRoutes discovered:\n%s", repoName, routes)
	return c.complete(ctx, SystemOpenAPIPrompt, prompt)
}