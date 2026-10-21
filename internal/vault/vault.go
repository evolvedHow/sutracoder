// Package vault resolves API keys from the central worker-env Cloudflare
// Worker vault. It is used only when a key is absent from the environment; a
// vault outage degrades to "not configured" and never hangs (short timeout).
package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client talks to the worker-env vault HTTP API.
type Client struct {
	// BaseURL is the worker URL, e.g. https://worker-env-vault…workers.dev.
	BaseURL string
	// Token is the bearer token.
	Token string
	// HTTPClient overrides the default client (tests inject fakes).
	HTTPClient *http.Client
}

// FromEnv builds a Client from VAULT_URL/VAULT_TOKEN, falling back to parsing
// ~/docker/stack/.env when the variables are unset. Returns a nil Client (and
// nil error) when credentials are unavailable, so callers degrade gracefully.
func FromEnv() *Client {
	base := strings.TrimSpace(os.Getenv("VAULT_URL"))
	token := strings.TrimSpace(os.Getenv("VAULT_TOKEN"))
	if base == "" || token == "" {
		if envFile, err := os.UserHomeDir(); err == nil {
			path := envFile + "/docker/stack/.env"
			b, rerr := os.ReadFile(path)
			if rerr == nil {
				if base == "" {
					base = parseEnvFile(string(b), "VAULT_URL")
				}
				if token == "" {
					token = parseEnvFile(string(b), "VAULT_TOKEN")
				}
			}
		}
	}
	if base == "" || token == "" {
		return nil
	}
	return &Client{
		BaseURL:    strings.TrimSuffix(base, "/"),
		Token:      token,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// Get retrieves a secret value. contextName may be empty for the default
// context. Returns an error on any failure (network, 404, malformed JSON).
func (c *Client) Get(ctx context.Context, contextName, key string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("vault not configured")
	}
	path := "/secrets/" + key
	if contextName != "" {
		path = "/secrets/" + contextName + "/" + key
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("vault request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("vault HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var payload struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode vault response: %w", err)
	}
	if payload.Value == "" {
		return "", fmt.Errorf("vault returned empty value for %s/%s", contextName, key)
	}
	return payload.Value, nil
}

// ResolveKey returns an API key: the environment value if set, otherwise the
// vault secret from the provider context, then the default (empty) context.
// Returns "" when unresolvable.
func ResolveKey(ctx context.Context, envName, providerContext string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	c := FromEnv()
	if c == nil {
		return ""
	}
	if providerContext != "" {
		if v, err := c.Get(ctx, providerContext, envName); err == nil {
			return v
		}
	}
	if v, err := c.Get(ctx, "", envName); err == nil {
		return v
	}
	return ""
}

func parseEnvFile(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "="); i > 0 {
			if strings.TrimSpace(line[:i]) == key {
				return strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
			}
		}
	}
	return ""
}
