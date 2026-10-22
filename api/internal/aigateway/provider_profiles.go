package aigateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Provider type selects metadata authentication and discovery, never a caller
// supplied URL, credential source, price, or automatic inference capability.
func ProviderType(value string) string {
	if value == "" {
		return "openai_compatible"
	}
	if value == "openai_compatible" || value == "anthropic" || value == "gemini" {
		return value
	}
	return ""
}

func ProviderProtocolsValid(kind string, protocols []string) bool {
	kind = ProviderType(kind)
	if kind == "" || ValidateProtocols(protocols) != nil {
		return false
	}
	if kind == "openai_compatible" {
		return true
	}
	if len(protocols) != 1 {
		return false
	}
	return kind == "anthropic" && protocols[0] == Messages || kind == "gemini" && protocols[0] == Gemini
}

// Native model lists are explicitly bounded and all-or-nothing. A truncated
// catalog is not a successful test. One total deadline spans every metadata GET.
func (p *ProviderNetworkPolicy) DiscoverProviderModels(ctx context.Context, endpoint ResolvedProviderEndpoint, credential string, timeout time.Duration, kind string) ([]string, error) {
	kind = ProviderType(kind)
	if kind == "openai_compatible" {
		return p.DiscoverModels(ctx, endpoint, credential, timeout)
	}
	if kind == "" || p.ValidateResolved(endpoint) != nil || !validProviderSecret(credential) || timeout < 5*time.Second || timeout > 30*time.Second {
		return nil, ProviderProbeError{"provider_configuration_rejected", false}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	route := Route{ID: "native-provider-probe", Name: "Native model metadata", Provider: "openai_compatible", BaseURL: endpoint.BaseURL, UpstreamModel: "metadata-only", AllowedIPs: append([]string(nil), endpoint.AllowedIPs...), LabHTTP: endpoint.LabHTTP, MaxOutputTokens: 1}
	route, err := p.ConfigureRoute(endpoint, route, credential)
	if err != nil {
		return nil, ProviderProbeError{"provider_configuration_rejected", false}
	}
	client := Client(route, timeout)
	defer client.CloseIdleConnections()
	seen, cursors := map[string]bool{}, map[string]bool{}
	cursor, total := "", 0
	for page := 0; page < 8; page++ {
		path := endpoint.BaseURL + "/models"
		query := url.Values{}
		if kind == "gemini" {
			path = strings.TrimSuffix(endpoint.BaseURL, "/v1") + "/v1beta/models"
			query.Set("pageSize", "100")
			if cursor != "" {
				query.Set("pageToken", cursor)
			}
		} else {
			query.Set("limit", "100")
			if cursor != "" {
				query.Set("after_id", cursor)
			}
		}
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, path+"?"+query.Encode(), nil)
		r.Header.Set("Accept", "application/json")
		if kind == "gemini" {
			r.Header.Set("x-goog-api-key", credential)
		} else {
			r.Header.Set("x-api-key", credential)
			r.Header.Set("anthropic-version", "2023-06-01")
		}
		response, err := client.Do(r)
		if err != nil {
			var verification *tls.CertificateVerificationError
			if errors.As(err, &verification) {
				return nil, ProviderProbeError{"provider_tls_verification_failed", false}
			}
			return nil, ProviderProbeError{"provider_connection_unavailable", true}
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			code, retry := "provider_response_unavailable", response.StatusCode >= 500
			switch {
			case response.StatusCode == 401 || response.StatusCode == 403:
				code = "provider_authentication_rejected"
			case response.StatusCode >= 300 && response.StatusCode < 400:
				code = "provider_redirect_rejected"
			case response.StatusCode == 404:
				code = "provider_models_endpoint_missing"
			case response.StatusCode == 429:
				code, retry = "provider_rate_limited", true
			}
			return nil, ProviderProbeError{code, retry}
		}
		media, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		total += len(body)
		if readErr != nil {
			return nil, ProviderProbeError{"provider_model_metadata_incomplete", true}
		}
		if len(body) > 1<<20 || total > 4<<20 {
			return nil, ProviderProbeError{"provider_model_metadata_limit", false}
		}
		if mediaErr != nil || media != "application/json" || response.Header.Get("Content-Encoding") != "" {
			return nil, ProviderProbeError{"provider_model_metadata_rejected", false}
		}
		ids, next, err := nativeModelPage(body, kind)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if !upstreamModelID.MatchString(id) || strings.Contains(id, credential) {
				return nil, ProviderProbeError{"provider_model_metadata_rejected", false}
			}
			seen[id] = true
		}
		if len(seen) > 256 {
			return nil, ProviderProbeError{"provider_model_metadata_limit", false}
		}
		if next == "" {
			models := make([]string, 0, len(seen))
			for id := range seen {
				models = append(models, id)
			}
			sort.Strings(models)
			return models, nil
		}
		if len(ids) == 0 || len(next) > 2048 || strings.ContainsAny(next, "\r\n\x00") || strings.Contains(next, credential) || cursors[next] {
			return nil, ProviderProbeError{"provider_model_pagination_rejected", false}
		}
		cursors[next], cursor = true, next
	}
	return nil, ProviderProbeError{"provider_model_metadata_limit", false}
}

func nativeModelPage(body []byte, kind string) ([]string, string, error) {
	rejected := ProviderProbeError{"provider_model_metadata_rejected", false}
	if kind == "anthropic" {
		var data struct {
			Data []struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"data"`
			HasMore *bool  `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if json.Unmarshal(body, &data) != nil || data.Data == nil || data.HasMore == nil || len(data.Data) > 100 {
			return nil, "", rejected
		}
		ids := make([]string, 0, len(data.Data))
		for _, item := range data.Data {
			if item.Type != "model" {
				return nil, "", rejected
			}
			ids = append(ids, item.ID)
		}
		if *data.HasMore {
			if len(ids) == 0 || data.LastID != ids[len(ids)-1] {
				return nil, "", rejected
			}
			return ids, data.LastID, nil
		}
		return ids, "", nil
	}
	var data struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
		Next string `json:"nextPageToken"`
	}
	if json.Unmarshal(body, &data) != nil || data.Models == nil || len(data.Models) > 100 {
		return nil, "", rejected
	}
	ids := make([]string, 0, len(data.Models))
	for _, item := range data.Models {
		if !strings.HasPrefix(item.Name, "models/") {
			return nil, "", rejected
		}
		id := strings.TrimPrefix(item.Name, "models/")
		if !geminiModel.MatchString(id) {
			return nil, "", rejected
		}
		ids = append(ids, id)
	}
	return ids, data.Next, nil
}
