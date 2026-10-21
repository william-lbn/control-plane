package aigateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ProviderProbeError struct {
	Code      string
	Retryable bool
}

func (e ProviderProbeError) Error() string { return e.Code }

var upstreamModelID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,127}$`)

// Model discovery is bounded metadata GET, never a paid inference, a guessed
// compatibility assertion, or an automatic publication of public model aliases.
func (p *ProviderNetworkPolicy) DiscoverModels(ctx context.Context, endpoint ResolvedProviderEndpoint, credential string, timeout time.Duration) ([]string, error) {
	if p.ValidateResolved(endpoint) != nil || credential != "" && !validProviderSecret(credential) || timeout < 5*time.Second || timeout > 30*time.Second {
		return nil, ProviderProbeError{"provider_configuration_rejected", false}
	}
	route := Route{ID: "provider-probe", Name: "Provider metadata probe", Provider: "openai_compatible", BaseURL: endpoint.BaseURL, UpstreamModel: "metadata-only", AllowedIPs: append([]string(nil), endpoint.AllowedIPs...), LabHTTP: endpoint.LabHTTP, MaxOutputTokens: 1}
	route, configurationErr := p.ConfigureRoute(endpoint, route, credential)
	if configurationErr != nil {
		return nil, ProviderProbeError{"provider_configuration_rejected", false}
	}
	client := Client(route, timeout)
	defer client.CloseIdleConnections()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.BaseURL+"/models", nil)
	if err != nil {
		return nil, ProviderProbeError{"provider_configuration_rejected", false}
	}
	r.Header.Set("Accept", "application/json")
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	response, err := client.Do(r)
	if err != nil {
		var verification *tls.CertificateVerificationError
		if errors.As(err, &verification) {
			return nil, ProviderProbeError{"provider_tls_verification_failed", false}
		}
		return nil, ProviderProbeError{"provider_connection_unavailable", true}
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		switch {
		case response.StatusCode == 401 || response.StatusCode == 403:
			return nil, ProviderProbeError{"provider_authentication_rejected", false}
		case response.StatusCode >= 300 && response.StatusCode < 400:
			return nil, ProviderProbeError{"provider_redirect_rejected", false}
		case response.StatusCode == 404:
			return nil, ProviderProbeError{"provider_models_endpoint_missing", false}
		case response.StatusCode == 429:
			return nil, ProviderProbeError{"provider_rate_limited", true}
		default:
			return nil, ProviderProbeError{"provider_response_unavailable", response.StatusCode >= 500}
		}
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || response.Header.Get("Content-Encoding") != "" {
		return nil, ProviderProbeError{"provider_model_metadata_rejected", false}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return nil, ProviderProbeError{"provider_model_metadata_incomplete", true}
	}
	if len(body) > 1<<20 {
		return nil, ProviderProbeError{"provider_model_metadata_limit", false}
	}
	var data struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &data) != nil || data.Data == nil || len(data.Data) > 256 || data.Object != "" && data.Object != "list" {
		return nil, ProviderProbeError{"provider_model_metadata_rejected", false}
	}
	set := map[string]bool{}
	for _, model := range data.Data {
		if !upstreamModelID.MatchString(model.ID) || credential != "" && strings.Contains(model.ID, credential) {
			return nil, ProviderProbeError{"provider_model_metadata_rejected", false}
		}
		set[model.ID] = true
	}
	models := make([]string, 0, len(set))
	for id := range set {
		models = append(models, id)
	}
	sort.Strings(models)
	return models, nil
}
