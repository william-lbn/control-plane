// Package dataapi implements a branch-scoped authentication boundary in front
// of PostgREST. Provisioning, database roles and Console integration belong to
// the control-plane Driver and are separate acceptance gates.
package dataapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"regexp"
	"strings"
)

var branchPattern = regexp.MustCompile(`^br_[a-f0-9]{16}$`)
var rolePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// Config is an immutable, versioned operator input. Only public provider keys
// belong here; the delegation signing seed is loaded from a separate Secret.
// No URL, role, issuer or audience may be chosen by an application request.
type Config struct {
	Version                int           `json:"version"`
	DelegationIssuer       string        `json:"delegation_issuer"`
	AllowPlaintextUpstream bool          `json:"allow_plaintext_upstream"`
	Routes                 []RouteConfig `json:"routes"`
}

type RouteConfig struct {
	BranchID       string          `json:"branch_id"`
	Upstream       string          `json:"upstream"`
	Issuer         string          `json:"issuer"`
	Audience       string          `json:"audience"`
	DefaultRole    string          `json:"default_role"`
	AllowedRoles   []string        `json:"allowed_roles"`
	AllowedOrigins []string        `json:"allowed_origins"`
	JWKS           json.RawMessage `json:"jwks"`
}

type providerKey struct {
	algorithm string
	public    any
}
type route struct {
	config  RouteConfig
	target  *url.URL
	keys    map[string]providerKey
	roles   map[string]bool
	origins map[string]bool
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid configuration JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("configuration must contain one JSON object")
	}
	return nil
}

func ParseConfig(reader io.Reader) (Config, error) {
	data, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return Config{}, errors.New("configuration exceeds its read budget")
	}
	var config Config
	if err := decodeStrict(data, &config); err != nil {
		return Config{}, err
	}
	if _, err := compileRoutes(config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func validText(value string, max int) bool {
	return value != "" && len(value) <= max && !strings.ContainsAny(value, "\r\n\t ")
}

func safeRole(name string) bool {
	return rolePattern.MatchString(name) && name != "postgres" && name != "cloud_admin" &&
		name != "superuser" && name != "authenticator" &&
		!strings.HasPrefix(name, "pg_") && !strings.HasPrefix(name, "neon_") && !strings.HasPrefix(name, "control_")
}

func compileRoutes(config Config) (map[string]*route, error) {
	if config.Version != 1 || !validText(config.DelegationIssuer, 512) || len(config.Routes) == 0 || len(config.Routes) > 256 {
		return nil, errors.New("unsupported configuration version or size")
	}
	result := make(map[string]*route, len(config.Routes))
	audiences := map[string]bool{}
	for index, item := range config.Routes {
		if !branchPattern.MatchString(item.BranchID) || result[item.BranchID] != nil ||
			!validText(item.Issuer, 512) || !validText(item.Audience, 512) || audiences[item.Audience] {
			return nil, fmt.Errorf("route %d has invalid or ambiguous branch scope", index)
		}
		audiences[item.Audience] = true
		target, err := url.Parse(item.Upstream)
		if err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" ||
			(target.Path != "" && target.Path != "/") || target.RawPath != "" ||
			(target.Scheme != "https" && !(config.AllowPlaintextUpstream && target.Scheme == "http")) {
			return nil, fmt.Errorf("route %d has invalid upstream", index)
		}
		if len(item.AllowedRoles) == 0 || len(item.AllowedRoles) > 32 || !safeRole(item.DefaultRole) {
			return nil, fmt.Errorf("route %d has invalid role policy", index)
		}
		r := &route{config: item, target: target, roles: map[string]bool{}, origins: map[string]bool{}}
		for _, role := range item.AllowedRoles {
			if !safeRole(role) || r.roles[role] {
				return nil, fmt.Errorf("route %d has invalid role policy", index)
			}
			r.roles[role] = true
		}
		if !r.roles[item.DefaultRole] || len(item.AllowedOrigins) > 32 {
			return nil, fmt.Errorf("route %d has invalid defaults", index)
		}
		for _, origin := range item.AllowedOrigins {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" ||
				u.RawQuery != "" || u.Fragment != "" || r.origins[origin] {
				return nil, fmt.Errorf("route %d has invalid CORS origin", index)
			}
			r.origins[origin] = true
		}
		r.keys, err = parseJWKS(item.JWKS)
		if err != nil {
			return nil, fmt.Errorf("route %d: %w", index, err)
		}
		result[item.BranchID] = r
	}
	return result, nil
}

func parseJWKS(data []byte) (map[string]providerKey, error) {
	var set struct {
		Keys []struct {
			KID string          `json:"kid"`
			KTY string          `json:"kty"`
			Use json.RawMessage `json:"use"`
			Alg string          `json:"alg"`
			Ops []string        `json:"key_ops"`
			CRV string          `json:"crv"`
			X   string          `json:"x"`
			N   string          `json:"n"`
			E   string          `json:"e"`
		} `json:"keys"`
	}
	if len(data) > 65536 || decodeStrict(data, &set) != nil || len(set.Keys) == 0 || len(set.Keys) > 32 {
		return nil, errors.New("invalid public provider JWKS")
	}
	keys := map[string]providerKey{}
	for _, key := range set.Keys {
		// RFC 7517 makes use optional. Better Auth emits signature JWKs without
		// it; a present value must still be sig. Algorithm, curve and private-key
		// rejection remain mandatory, so encryption keys cannot be used.
		var use string
		invalidUse := len(key.Use) != 0 && (json.Unmarshal(key.Use, &use) != nil || use != "sig")
		if !validText(key.KID, 128) || keys[key.KID].public != nil || invalidUse {
			return nil, errors.New("provider key identity or use is invalid")
		}
		if len(key.Ops) > 1 || (len(key.Ops) == 1 && key.Ops[0] != "verify") {
			return nil, errors.New("provider keys must be verify-only")
		}
		switch {
		case key.KTY == "OKP" && key.CRV == "Ed25519" && key.Alg == "EdDSA" && key.N == "" && key.E == "":
			public, err := base64.RawURLEncoding.Strict().DecodeString(key.X)
			if err != nil || len(public) != ed25519.PublicKeySize {
				return nil, errors.New("invalid Ed25519 public key")
			}
			keys[key.KID] = providerKey{key.Alg, ed25519.PublicKey(public)}
		case key.KTY == "RSA" && key.Alg == "RS256" && key.CRV == "" && key.X == "":
			n, err := base64.RawURLEncoding.Strict().DecodeString(key.N)
			e, expErr := base64.RawURLEncoding.Strict().DecodeString(key.E)
			modulus := new(big.Int).SetBytes(n)
			if err != nil || expErr != nil || len(e) == 0 || len(e) > 4 || e[0] == 0 || len(n) == 0 || n[0] == 0 ||
				modulus.BitLen() < 2048 || modulus.BitLen() > 4096 {
				return nil, errors.New("invalid RSA public key size")
			}
			exponent := new(big.Int).SetBytes(e).Int64()
			if exponent < 3 || exponent > 2147483647 || exponent%2 == 0 {
				return nil, errors.New("invalid RSA exponent")
			}
			keys[key.KID] = providerKey{key.Alg, &rsa.PublicKey{N: modulus, E: int(exponent)}}
		default:
			return nil, errors.New("unsupported provider signing key")
		}
	}
	return keys, nil
}
