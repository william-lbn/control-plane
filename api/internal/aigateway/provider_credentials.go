package aigateway

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"

	"golang.org/x/crypto/nacl/box"
)

// The database stores only anonymous sealed boxes. API admission requires the
// public key; opening belongs to the trusted validator/inference processes.
// This protects database rows/backups, not a compromised Kubernetes administrator.
type ProviderCredentialScope struct {
	ProviderID     string `json:"provider_id"`
	RevisionID     string `json:"revision_id"`
	EndpointSHA256 string `json:"endpoint_sha256"`
}

type SealedProviderCredential struct {
	Version    int    `json:"version"`
	KeyVersion string `json:"key_version"`
	Ciphertext string `json:"ciphertext"`
}

type ProviderSealer struct {
	active string
	public map[string]*[32]byte
}
type ProviderOpener struct {
	sealer  *ProviderSealer
	private map[string]*[32]byte
}

func (*ProviderSealer) String() string   { return "[provider public keyring]" }
func (*ProviderSealer) GoString() string { return "[provider public keyring]" }
func (*ProviderOpener) String() string   { return "[private provider keyring redacted]" }
func (*ProviderOpener) GoString() string { return "[private provider keyring redacted]" }

var providerIDPattern = regexp.MustCompile(`^aip_[a-f0-9]{24}$`)
var providerRevisionPattern = regexp.MustCompile(`^apv_[a-f0-9]{24}$`)
var providerEndpointHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var providerKeyVersionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var errProviderCredential = errors.New("provider credential unavailable or scope rejected")

func (s ProviderCredentialScope) valid() bool {
	return providerIDPattern.MatchString(s.ProviderID) && providerRevisionPattern.MatchString(s.RevisionID) && providerEndpointHashPattern.MatchString(s.EndpointSHA256)
}
func validProviderSecret(value string) bool {
	if len(value) < 16 || len(value) > 8192 {
		return false
	}
	for _, b := range []byte(value) {
		if b < 33 || b > 126 {
			return false
		}
	}
	return true
}

type providerPublicKeyset struct {
	Version int               `json:"version"`
	Active  string            `json:"active"`
	Keys    map[string]string `json:"public_keys"`
}
type providerPrivateKeyset struct {
	Version int               `json:"version"`
	Active  string            `json:"active"`
	Keys    map[string]string `json:"private_keys"`
}
type providerCredentialPayload struct {
	Scope      ProviderCredentialScope `json:"scope"`
	KeyVersion string                  `json:"key_version"`
	Secret     string                  `json:"secret"`
}

func providerJSON(raw []byte, v any) error {
	if len(raw) == 0 || len(raw) > 32<<10 {
		return errProviderCredential
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return errProviderCredential
	}
	return nil
}

func providerKey(raw string) (*[32]byte, error) {
	data, err := base64.StdEncoding.Strict().DecodeString(raw)
	if err != nil || len(data) != 32 || base64.StdEncoding.EncodeToString(data) != raw || bytes.Equal(data, make([]byte, 32)) {
		return nil, errProviderCredential
	}
	key := new([32]byte)
	copy(key[:], data)
	clear(data)
	return key, nil
}

func NewProviderSealer(raw []byte) (*ProviderSealer, error) {
	var set providerPublicKeyset
	if providerJSON(raw, &set) != nil || set.Version != 1 || !providerKeyVersionPattern.MatchString(set.Active) || len(set.Keys) < 1 || len(set.Keys) > 8 {
		return nil, errProviderCredential
	}
	s := &ProviderSealer{active: set.Active, public: map[string]*[32]byte{}}
	for version, value := range set.Keys {
		if !providerKeyVersionPattern.MatchString(version) {
			return nil, errProviderCredential
		}
		key, err := providerKey(value)
		if err != nil {
			return nil, err
		}
		s.public[version] = key
	}
	if s.public[s.active] == nil {
		return nil, errProviderCredential
	}
	return s, nil
}

func NewProviderOpener(raw []byte) (*ProviderOpener, error) {
	var set providerPrivateKeyset
	if providerJSON(raw, &set) != nil || set.Version != 1 || !providerKeyVersionPattern.MatchString(set.Active) || len(set.Keys) < 1 || len(set.Keys) > 8 {
		return nil, errProviderCredential
	}
	s := &ProviderSealer{active: set.Active, public: map[string]*[32]byte{}}
	o := &ProviderOpener{sealer: s, private: map[string]*[32]byte{}}
	for version, value := range set.Keys {
		if !providerKeyVersionPattern.MatchString(version) {
			return nil, errProviderCredential
		}
		key, err := providerKey(value)
		if err != nil {
			return nil, err
		}
		private, err := ecdh.X25519().NewPrivateKey(key[:])
		if err != nil {
			return nil, errProviderCredential
		}
		public := new([32]byte)
		copy(public[:], private.PublicKey().Bytes())
		o.private[version], s.public[version] = key, public
	}
	if o.private[s.active] == nil {
		return nil, errProviderCredential
	}
	return o, nil
}

func (o *ProviderOpener) PublicKeyset() ([]byte, error) {
	if o == nil || o.sealer == nil {
		return nil, errProviderCredential
	}
	return o.sealer.PublicKeyset()
}

func (s *ProviderSealer) PublicKeyset() ([]byte, error) {
	if s == nil || s.public[s.active] == nil {
		return nil, errProviderCredential
	}
	keys := map[string]string{}
	for version, key := range s.public {
		keys[version] = base64.StdEncoding.EncodeToString(key[:])
	}
	return json.Marshal(providerPublicKeyset{Version: 1, Active: s.active, Keys: keys})
}

// GenerateProviderKeysets is for an operator's exclusive private output files.
// The returned private JSON must never be logged or mounted into the Console.
func GenerateProviderKeysets(version string) (publicJSON, privateJSON []byte, err error) {
	if !providerKeyVersionPattern.MatchString(version) {
		return nil, nil, errProviderCredential
	}
	public, private, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, errProviderCredential
	}
	defer clear(private[:])
	pub, err := json.Marshal(providerPublicKeyset{Version: 1, Active: version, Keys: map[string]string{version: base64.StdEncoding.EncodeToString(public[:])}})
	if err != nil {
		return nil, nil, errProviderCredential
	}
	priv, err := json.Marshal(providerPrivateKeyset{Version: 1, Active: version, Keys: map[string]string{version: base64.StdEncoding.EncodeToString(private[:])}})
	if err != nil {
		return nil, nil, errProviderCredential
	}
	return pub, priv, nil
}

func (s *ProviderSealer) Seal(scope ProviderCredentialScope, secret string) (SealedProviderCredential, error) {
	var empty SealedProviderCredential
	if s == nil || !scope.valid() || !validProviderSecret(secret) || s.public[s.active] == nil {
		return empty, errProviderCredential
	}
	plain, err := json.Marshal(providerCredentialPayload{Scope: scope, KeyVersion: s.active, Secret: secret})
	if err != nil {
		return empty, errProviderCredential
	}
	defer clear(plain)
	sealed, err := box.SealAnonymous(nil, plain, s.public[s.active], rand.Reader)
	if err != nil {
		return empty, errProviderCredential
	}
	return SealedProviderCredential{Version: 1, KeyVersion: s.active, Ciphertext: base64.StdEncoding.EncodeToString(sealed)}, nil
}

// The authenticated payload includes immutable scope and recipient version;
// moving a database ciphertext to another provider/revision/endpoint fails.
func (o *ProviderOpener) Open(scope ProviderCredentialScope, sealed SealedProviderCredential) (string, error) {
	if o == nil || o.sealer == nil || !scope.valid() || sealed.Version != 1 || !providerKeyVersionPattern.MatchString(sealed.KeyVersion) || len(sealed.Ciphertext) > 16<<10 {
		return "", errProviderCredential
	}
	private, public := o.private[sealed.KeyVersion], o.sealer.public[sealed.KeyVersion]
	if private == nil || public == nil {
		return "", errProviderCredential
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(sealed.Ciphertext)
	if err != nil || len(raw) < box.AnonymousOverhead || base64.StdEncoding.EncodeToString(raw) != sealed.Ciphertext {
		return "", errProviderCredential
	}
	plain, ok := box.OpenAnonymous(nil, raw, public, private)
	if !ok {
		return "", errProviderCredential
	}
	defer clear(plain)
	var payload providerCredentialPayload
	if providerJSON(plain, &payload) != nil || payload.Scope != scope || payload.KeyVersion != sealed.KeyVersion || !validProviderSecret(payload.Secret) {
		return "", errProviderCredential
	}
	return payload.Secret, nil
}
