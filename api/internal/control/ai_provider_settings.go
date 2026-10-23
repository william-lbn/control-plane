package control

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

type aiProviderCredentialSource struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Namespace       string   `json:"namespace"`
	SecretName      string   `json:"secret_name"`
	SecretUID       string   `json:"secret_uid"`
	DataKey         string   `json:"data_key"`
	AllowedBaseURLs []string `json:"allowed_base_urls"`
}
type aiProviderPolicyInput struct {
	AllowOrganizationBYOK bool                                 `json:"allow_organization_byok,omitempty"`
	Version               int                                  `json:"version"`
	OperatorIDs           []string                             `json:"operator_ids"`
	ApprovedEndpoints     []aigateway.ApprovedProviderEndpoint `json:"approved_endpoints"`
	CredentialSources     []aiProviderCredentialSource         `json:"credential_sources"`
	AllowLabSecretInput   bool                                 `json:"allow_lab_secret_input"`
	EgressProxyURL        string                               `json:"egress_proxy_url,omitempty"`
}
type aiProviderSettings struct {
	policy       aiProviderPolicyInput
	operators    map[string]bool
	sources      map[string]aiProviderCredentialSource
	network      *aigateway.ProviderNetworkPolicy
	sealer       *aigateway.ProviderSealer
	opener       *aigateway.ProviderOpener
	policySHA256 string
}

func (*aiProviderSettings) String() string {
	return "[operator provider settings; private material redacted]"
}
func (*aiProviderSettings) GoString() string {
	return "[operator provider settings; private material redacted]"
}

var aiProviderSourceID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var aiProviderUserID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
var aiProviderNamespace = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
var aiProviderSecretName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
var aiProviderSourceKey = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,253}$`)
var errAIProviderSettings = errors.New("approved provider management policy and key projection required")

func aiProviderBoundedJSON(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > 128<<10 {
		return errAIProviderSettings
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		return errAIProviderSettings
	}
	return nil
}

func newAIProviderSettings(policyRaw, publicRaw, privateRaw []byte) (*aiProviderSettings, error) {
	var policy aiProviderPolicyInput
	if aiProviderBoundedJSON(policyRaw, &policy) != nil || policy.Version != 1 || len(policy.OperatorIDs) < 1 || len(policy.OperatorIDs) > 16 || len(policy.CredentialSources) > 16 {
		return nil, errAIProviderSettings
	}
	network, err := aigateway.NewProviderNetworkPolicy(policy.ApprovedEndpoints)
	if err != nil {
		return nil, errAIProviderSettings
	}
	network, err = network.WithEgressProxy(policy.EgressProxyURL)
	if err != nil {
		return nil, errAIProviderSettings
	}
	sealer, err := aigateway.NewProviderSealer(publicRaw)
	if err != nil {
		return nil, errAIProviderSettings
	}
	settings := &aiProviderSettings{policy: policy, operators: map[string]bool{}, sources: map[string]aiProviderCredentialSource{}, network: network, sealer: sealer}
	for _, id := range policy.OperatorIDs {
		if !aiProviderUserID.MatchString(id) || settings.operators[id] {
			return nil, errAIProviderSettings
		}
		settings.operators[id] = true
	}
	for _, source := range policy.CredentialSources {
		if !aiProviderSourceID.MatchString(source.ID) || len(source.Name) < 1 || len(source.Name) > 100 || !aiProviderNamespace.MatchString(source.Namespace) || source.Namespace != env("NEON_KUBE_NAMESPACE", "neon") || !aiProviderSecretName.MatchString(source.SecretName) || !functionResourceUID.MatchString(source.SecretUID) || !aiProviderSourceKey.MatchString(source.DataKey) || len(source.AllowedBaseURLs) < 1 || len(source.AllowedBaseURLs) > 8 {
			return nil, errAIProviderSettings
		}
		if _, exists := settings.sources[source.ID]; exists {
			return nil, errAIProviderSettings
		}
		seen := map[string]bool{}
		for _, raw := range source.AllowedBaseURLs {
			base, err := aigateway.CanonicalProviderURL(raw)
			if err != nil || seen[base] {
				return nil, errAIProviderSettings
			}
			seen[base] = true
		}
		source.AllowedBaseURLs = nil
		for value := range seen {
			source.AllowedBaseURLs = append(source.AllowedBaseURLs, value)
		}
		sort.Strings(source.AllowedBaseURLs)
		settings.sources[source.ID] = source
	}
	if len(privateRaw) > 0 {
		settings.opener, err = aigateway.NewProviderOpener(privateRaw)
		if err != nil {
			return nil, errAIProviderSettings
		}
		public, err := sealer.PublicKeyset()
		if err != nil {
			return nil, errAIProviderSettings
		}
		fromPrivate, err := settings.opener.PublicKeyset()
		if err != nil || !bytes.Equal(public, fromPrivate) {
			return nil, errAIProviderSettings
		}
	}
	normalized, _ := json.Marshal(policy)
	hash := sha256.Sum256(normalized)
	settings.policySHA256 = hex.EncodeToString(hash[:])
	return settings, nil
}

func aiProviderSettingFile(path string, limit int64) ([]byte, error) {
	if path == "" {
		return nil, errAIProviderSettings
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errAIProviderSettings
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(raw) == 0 || int64(len(raw)) > limit {
		return nil, errAIProviderSettings
	}
	return raw, nil
}

func loadAIProviderSettings(private bool) (*aiProviderSettings, error) {
	if os.Getenv("NEON_AI_PROVIDER_MANAGEMENT_ENABLED") != "true" {
		return nil, nil
	}
	policy, err := aiProviderSettingFile(os.Getenv("NEON_AI_PROVIDER_POLICY_FILE"), 128<<10)
	if err != nil {
		return nil, err
	}
	public, err := aiProviderSettingFile(os.Getenv("NEON_AI_PROVIDER_PUBLIC_KEYRING_FILE"), 32<<10)
	if err != nil {
		return nil, err
	}
	var secret []byte
	if private {
		secret, err = aiProviderSettingFile(os.Getenv("NEON_AI_PROVIDER_PRIVATE_KEYRING_FILE"), 32<<10)
		if err != nil {
			return nil, err
		}
		defer clear(secret)
	}
	return newAIProviderSettings(policy, public, secret)
}

func (p *aiProviderSettings) isOperator(actor user) bool {
	return p != nil && !actor.APIKey && p.operators[actor.ID]
}

func (p *aiProviderSettings) permitsSecretInput(r *http.Request, baseURL string, labAcknowledged bool, secureCookies bool) bool {
	if p == nil {
		return false
	}
	if secureCookies && r.TLS != nil && r.TLS.HandshakeComplete && r.TLS.Version >= tls.VersionTLS12 {
		return true
	}
	return p.policy.AllowLabSecretInput && labAcknowledged && p.network.IsApprovedLabEndpoint(baseURL)
}

func (p *aiProviderSettings) sourceFor(id, baseURL string) (aiProviderCredentialSource, bool) {
	if p == nil {
		return aiProviderCredentialSource{}, false
	}
	source, ok := p.sources[id]
	if !ok {
		return source, false
	}
	for _, approved := range source.AllowedBaseURLs {
		if strings.TrimRight(baseURL, "/") == approved {
			return source, true
		}
	}
	return source, false
}
