// Package aigateway implements the bounded OpenAI-compatible inference adapter.
// Routes are operator-approved inputs, never URLs supplied by application callers.
package aigateway

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
)

type Route struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Provider        string   `json:"provider"`
	BaseURL         string   `json:"base_url"`
	UpstreamModel   string   `json:"upstream_model"`
	APIKeyFile      string   `json:"api_key_file,omitempty"`
	AllowedIPs      []string `json:"allowed_ips"`
	LabHTTP         bool     `json:"lab_allow_http"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	key             string
	egressProxy     string
}
type Config struct {
	Version                int     `json:"version"`
	Generation             int     `json:"generation"`
	TimeoutSeconds         int     `json:"timeout_seconds"`
	MaxConcurrentPerBranch int     `json:"max_concurrent_per_branch"`
	RequestsPerMinute      int     `json:"requests_per_minute"`
	Routes                 []Route `json:"routes"`
}

// WithProviderCredential keeps the opened key outside JSON/public route fields.
// Callers must validate the endpoint against their current provider policy first.
func (r Route) WithProviderCredential(secret string) (Route, error) {
	if secret != "" && !validProviderSecret(secret) {
		return Route{}, errProviderCredential
	}
	r.key = secret
	return r, nil
}

var modelID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]{0,127}$`)

func LoadPublic(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("AI route configuration unavailable")
	}
	var c Config
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if len(raw) > 65536 || d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid AI route configuration")
	}
	if err = c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
func Load(path string) (*Config, error) {
	c, e := LoadPublic(path)
	if e != nil {
		return nil, e
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.APIKeyFile != "" {
			b, e := os.ReadFile(r.APIKeyFile)
			if e != nil {
				return nil, errors.New("AI provider credential unavailable")
			}
			r.key = strings.TrimSpace(string(b))
			if len(r.key) < 16 || len(r.key) > 8192 || strings.ContainsAny(r.key, "\r\n") {
				return nil, errors.New("invalid AI provider credential")
			}
		}
	}
	return c, nil
}
func (c *Config) Validate() error {
	if c.Version != 1 || c.Generation < 1 || c.TimeoutSeconds < 5 || c.TimeoutSeconds > 180 || c.MaxConcurrentPerBranch < 1 || c.MaxConcurrentPerBranch > 16 || c.RequestsPerMinute < 1 || c.RequestsPerMinute > 10000 || len(c.Routes) > 100 {
		return errors.New("invalid AI configuration bounds")
	}
	seen := map[string]bool{}
	for _, r := range c.Routes {
		u, e := url.Parse(r.BaseURL)
		if !modelID.MatchString(r.ID) || seen[r.ID] || len(r.Name) < 1 || len(r.Name) > 100 || len(r.UpstreamModel) < 1 || len(r.UpstreamModel) > 128 || r.Provider != "openai_compatible" || e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.Path, "/") != "/v1" || !(u.Scheme == "https" || u.Scheme == "http" && r.LabHTTP) || r.MaxOutputTokens < 1 || r.MaxOutputTokens > 4096 || len(r.AllowedIPs) < 1 || len(r.AllowedIPs) > 8 {
			return errors.New("invalid AI model route")
		}
		for _, ip := range r.AllowedIPs {
			if net.ParseIP(ip) == nil {
				return errors.New("AI route requires pinned provider IPs")
			}
		}
		seen[r.ID] = true
	}
	return nil
}
func (c *Config) Find(id string) (Route, bool) {
	for _, r := range c.Routes {
		if r.ID == id {
			return r, true
		}
	}
	return Route{}, false
}
func (c *Config) PublicModels() []map[string]any {
	items := []map[string]any{}
	for _, r := range c.Routes {
		items = append(items, map[string]any{"id": r.ID, "object": "model", "name": r.Name, "owned_by": r.Provider, "protocols": []string{"chat_completions"}, "max_output_tokens": r.MaxOutputTokens})
	}
	return items
}
