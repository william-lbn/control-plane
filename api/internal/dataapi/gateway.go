package dataapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const maxBodyBytes = 1 << 20
const maxTokenBytes = 16384

// Gateway accepts immutable, operator-owned routes. It never reads a Console
// session, a database password or an application-supplied destination URL.
type Gateway struct {
	routes      map[string]*route
	proxies     map[string]*httputil.ReverseProxy
	signer      ed25519.PrivateKey
	issuer, kid string
	logger      *slog.Logger
	capacity    chan struct{}
	now         func() time.Time
}

type delegationContextKey struct{}

func New(config Config, seed []byte, logger *slog.Logger) (*Gateway, error) {
	return NewWithTrust(config, seed, logger, nil)
}

// NewWithTrust augments system roots with an operator-provided upstream CA.
// Hostname verification remains mandatory for HTTPS; no skip-verify exists.
func NewWithTrust(config Config, seed []byte, logger *slog.Logger, ca []byte) (*Gateway, error) {
	if len(seed) != ed25519.SeedSize || logger == nil {
		return nil, errors.New("delegation seed and logger required")
	}
	routes, err := compileRoutes(config)
	if err != nil {
		return nil, err
	}
	key := ed25519.NewKeyFromSeed(seed)
	digest := sha256.Sum256(key.Public().(ed25519.PublicKey))
	g := &Gateway{routes: routes, proxies: map[string]*httputil.ReverseProxy{},
		signer: key, issuer: config.DelegationIssuer, kid: hex.EncodeToString(digest[:16]),
		logger: logger, capacity: make(chan struct{}, 64), now: time.Now}
	var roots *x509.CertPool
	if len(ca) > 0 {
		roots, err = x509.SystemCertPool()
		if err != nil {
			return nil, errors.New("system trust unavailable")
		}
		if !roots.AppendCertsFromPEM(ca) {
			return nil, errors.New("upstream CA rejected")
		}
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 130 * time.Second, IdleConnTimeout: 30 * time.Second,
		MaxIdleConns: 64, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 8, DisableCompression: true}
	for branch, r := range routes {
		target := r.target
		g.proxies[branch] = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = target.Host
				// Set this after ReverseProxy removes hop-by-hop headers: a
				// caller's Connection: Authorization must not erase our identity.
				delegated, _ := pr.In.Context().Value(delegationContextKey{}).(string)
				pr.Out.Header.Set("Authorization", "Bearer "+delegated)
				for _, name := range []string{"Cookie", "Proxy-Authorization", "X-Original-URL", "X-Rewrite-URL", "X-Forwarded-User", "X-Neon-Branch", "X-Neon-Role"} {
					pr.Out.Header.Del(name)
				}
			},
			Transport: transport,
			ErrorLog:  slog.NewLogLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelError),
			ErrorHandler: func(w http.ResponseWriter, req *http.Request, _ error) {
				g.logger.Warn("data API upstream unavailable", "branch_id", branch)
				writeError(w, 503, "upstream_unavailable")
			},
			ModifyResponse: func(response *http.Response) error {
				response.Header.Del("Set-Cookie")
				for key := range response.Header {
					if strings.HasPrefix(strings.ToLower(key), "access-control-") {
						response.Header.Del(key)
					}
				}
				response.Header.Set("Cache-Control", "no-store")
				response.Header.Set("X-Content-Type-Options", "nosniff")
				return nil
			},
		}
	}
	return g, nil
}

// DelegationJWKS exports only the verification key. PostgREST must trust this
// exact key and accept traffic solely from this gateway's workload identity.
func (g *Gateway) DelegationJWKS() []byte {
	data, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "key_ops": []string{"verify"},
		"kid": g.kid, "x": base64.RawURLEncoding.EncodeToString(g.signer.Public().(ed25519.PublicKey)),
	}}})
	return data
}

func (g *Gateway) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	// Configuration readiness only. Driver readiness must separately observe
	// PostgREST/SQL/RLS; this endpoint deliberately does not cold wake a Compute.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	return mux
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if status == 401 {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	// Require the canonical public route. Escaped separators, dot paths and
	// alternate spellings may otherwise be interpreted differently downstream.
	path := request.URL.Path
	if strings.Contains(request.URL.EscapedPath(), "%") || len(request.URL.RawQuery) > 16384 {
		writeError(w, 400, "invalid_request_path")
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/data/v1/"), "/", 2)
	if !strings.HasPrefix(path, "/data/v1/") || len(parts) != 2 || !branchPattern.MatchString(parts[0]) ||
		strings.Contains(parts[1], "//") || strings.Contains(parts[1], "\\") {
		writeError(w, 404, "route_not_found")
		return
	}
	for _, segment := range strings.Split(parts[1], "/") {
		if segment == "." || segment == ".." {
			writeError(w, 400, "invalid_request_path")
			return
		}
	}
	r := g.routes[parts[0]]
	if r == nil {
		writeError(w, 404, "route_not_found")
		return
	}
	if !allowedMethod(request.Method) {
		writeError(w, 405, "method_not_allowed")
		return
	}
	if len(request.Header.Values("Origin")) > 1 {
		writeError(w, 403, "origin_not_allowed")
		return
	}
	origin := request.Header.Get("Origin")
	if origin != "" {
		if !r.origins[origin] {
			writeError(w, 403, "origin_not_allowed")
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Range,Range-Unit,Preference-Applied")
	}
	if request.Method == http.MethodOptions {
		if origin == "" || !allowedMethod(request.Header.Get("Access-Control-Request-Method")) ||
			request.Header.Get("Access-Control-Request-Method") == http.MethodOptions || !allowedCORSHeaders(request.Header.Get("Access-Control-Request-Headers")) {
			writeError(w, 403, "invalid_preflight")
			return
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET,HEAD,POST,PATCH,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type,Prefer,Range,Range-Unit,Accept-Profile,Content-Profile")
		w.Header().Set("Access-Control-Max-Age", "300")
		w.WriteHeader(204)
		return
	}
	claims, err := g.authenticate(r, request.Header.Values("Authorization"))
	if err != nil {
		writeError(w, 401, "invalid_token")
		return
	}
	select {
	case g.capacity <- struct{}{}:
		defer func() { <-g.capacity }()
	default:
		writeError(w, 429, "gateway_capacity_exceeded")
		return
	}
	if request.ContentLength > maxBodyBytes {
		writeError(w, 413, "request_body_too_large")
		return
	}
	var body []byte
	if request.Body != nil {
		body, err = io.ReadAll(io.LimitReader(request.Body, maxBodyBytes+1))
		if err != nil {
			writeError(w, 400, "invalid_request_body")
			return
		}
		if len(body) > maxBodyBytes {
			writeError(w, 413, "request_body_too_large")
			return
		}
	}
	delegated, err := g.delegate(r, claims)
	if err != nil {
		writeError(w, 503, "delegation_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithValue(request.Context(), delegationContextKey{}, delegated), 150*time.Second)
	defer cancel()
	out := request.Clone(ctx)
	out.URL.Path = "/" + parts[1]
	out.URL.RawPath = ""
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	out.TransferEncoding = nil
	g.proxies[r.config.BranchID].ServeHTTP(w, out)
}

func allowedMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
}
func allowedCORSHeaders(headers string) bool {
	for _, header := range strings.Split(headers, ",") {
		switch strings.ToLower(strings.TrimSpace(header)) {
		case "", "authorization", "content-type", "prefer", "range", "range-unit", "accept-profile", "content-profile":
		default:
			return false
		}
	}
	return true
}

func (g *Gateway) authenticate(r *route, headers []string) (jwt.MapClaims, error) {
	if len(headers) != 1 || len(headers[0]) > maxTokenBytes+7 {
		return nil, errors.New("invalid bearer")
	}
	parts := strings.Split(headers[0], " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" || len(parts[1]) > maxTokenBytes {
		return nil, errors.New("invalid bearer")
	}
	token, err := jwt.Parse(parts[1], func(token *jwt.Token) (any, error) {
		for _, unsupported := range []string{"crit", "jku", "jwk", "x5u", "x5c"} {
			if _, exists := token.Header[unsupported]; exists {
				return nil, errors.New("unsupported token header")
			}
		}
		kid, ok := token.Header["kid"].(string)
		key := r.keys[kid]
		if !ok || key.public == nil || key.algorithm != token.Method.Alg() {
			return nil, errors.New("unknown signing key")
		}
		return key.public, nil
	}, jwt.WithValidMethods([]string{"RS256", "EdDSA"}), jwt.WithExpirationRequired(),
		jwt.WithIssuer(r.config.Issuer), jwt.WithAudience(r.config.Audience), jwt.WithIssuedAt(),
		jwt.WithJSONNumber(), jwt.WithStrictDecoding(), jwt.WithTimeFunc(g.now))
	if err != nil || !token.Valid {
		return nil, errors.New("token rejected")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid claims")
	}
	subject, err := claims.GetSubject()
	if err != nil || !validText(subject, 256) {
		return nil, errors.New("subject required")
	}
	audience, err := claims.GetAudience()
	if err != nil || len(audience) != 1 || audience[0] != r.config.Audience {
		return nil, errors.New("single branch audience required")
	}
	if branch, exists := claims["branch_id"]; exists && branch != r.config.BranchID {
		return nil, errors.New("branch scope rejected")
	}
	role := r.config.DefaultRole
	if value, exists := claims["role"]; exists {
		var ok bool
		role, ok = value.(string)
		if !ok {
			return nil, errors.New("role rejected")
		}
	}
	if !r.roles[role] {
		return nil, errors.New("role rejected")
	}
	claims["role"] = role
	return claims, nil
}

func (g *Gateway) delegate(r *route, claims jwt.MapClaims) (string, error) {
	now := g.now()
	expires, err := claims.GetExpirationTime()
	if err != nil || expires == nil || !expires.After(now) {
		return "", errors.New("expired token")
	}
	exp := now.Add(30 * time.Second)
	if expires.Before(exp) {
		exp = expires.Time
	}
	// Preserve verified provider claims for RLS, but never let the application
	// select the trusted hop identity, branch or lifetime.
	copy := jwt.MapClaims{}
	for key, value := range claims {
		copy[key] = value
	}
	copy["provider_issuer"] = r.config.Issuer
	copy["iss"], copy["aud"], copy["branch_id"] = g.issuer, r.config.Audience, r.config.BranchID
	copy["iat"], copy["nbf"], copy["exp"] = now.Unix(), now.Unix(), exp.Unix()
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, copy)
	token.Header["kid"] = g.kid
	return token.SignedString(g.signer)
}
