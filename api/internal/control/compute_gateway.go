package control

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"
)

var computeWorkloadPattern = regexp.MustCompile(`^cp-[a-f0-9]{16}$`)

type computeGateway struct {
	kube *kubeClient
	http *http.Client
}

// RunComputeGateway protects the pinned native plain HTTP API with verified
// TLS and an exact-endpoint JWT. Only configure is exposed. The native JWT
// check remains enabled. This service never accepts caller-provided targets.
func RunComputeGateway(ctx context.Context) error {
	kube, err := newKubeClient()
	if err != nil {
		return err
	}
	gateway := &computeGateway{kube: kube, http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /computes/{compute}/configure", gateway.configure)
	server := &http.Server{Addr: env("NEON_COMPUTE_GATEWAY_BIND", ":8443"), Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: gateway.certificate}}
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, record{"status": "ok"}) })
	healthMux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if _, err := kube.request(ctx, http.MethodGet, kube.path("service", "neon-compute-management-gateway"), nil); err != nil {
			jsonResponse(w, 503, record{"status": "unavailable"})
			return
		}
		jsonResponse(w, 200, record{"status": "ready"})
	})
	health := &http.Server{Addr: env("NEON_COMPUTE_GATEWAY_HEALTH_BIND", ":8081"), Handler: healthMux, ReadHeaderTimeout: 5 * time.Second}
	failures := make(chan error, 2)
	go func() { failures <- server.ListenAndServeTLS("", "") }()
	go func() { failures <- health.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-failures:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.Shutdown(shutdown)
	health.Shutdown(shutdown)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (g *computeGateway) config(ctx context.Context, workload string) (map[string]any, error) {
	if !computeWorkloadPattern.MatchString(workload) {
		return nil, errors.New("invalid compute identity")
	}
	secret, err := g.kube.request(ctx, http.MethodGet, g.kube.path("secret", workload+"-config"), nil)
	if err != nil {
		return nil, err
	}
	if nested(secret, "metadata", "labels", "neon-control/endpoint-id") != "ep_"+strings.TrimPrefix(workload, "cp-") {
		return nil, errors.New("compute config ownership mismatch")
	}
	return secret, nil
}
func (g *computeGateway) certificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	suffix := "-management." + g.kube.namespace + ".svc"
	if !strings.HasSuffix(hello.ServerName, suffix) {
		return nil, errors.New("endpoint SNI required")
	}
	ctx, cancel := context.WithTimeout(hello.Context(), 10*time.Second)
	defer cancel()
	secret, err := g.config(ctx, strings.TrimSuffix(hello.ServerName, suffix))
	if err != nil {
		return nil, err
	}
	cert, err := secretText(secret, "control.crt")
	if err != nil {
		return nil, err
	}
	key, err := secretText(secret, "control.key")
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair([]byte(cert), []byte(key))
	return &pair, err
}
func validComputeJWT(token, workload string, config map[string]any, now time.Time) bool {
	if len(token) > 4096 || !computeWorkloadPattern.MatchString(workload) {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(headerBytes, &header) != nil || header.Alg != "EdDSA" {
		return false
	}
	keys, ok := nested(config, "compute_ctl_config", "jwks", "keys").([]any)
	if !ok {
		return false
	}
	var public []byte
	for _, v := range keys {
		jwk, ok := v.(map[string]any)
		if !ok {
			return false
		}
		if jwk["kid"] == header.Kid && jwk["alg"] == "EdDSA" && jwk["kty"] == "OKP" && jwk["crv"] == "Ed25519" {
			public, err = base64.RawURLEncoding.DecodeString(stringVal(jwk["x"]))
			if err != nil {
				return false
			}
		}
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(public) != ed25519.PublicKeySize || !ed25519.Verify(ed25519.PublicKey(public), []byte(parts[0]+"."+parts[1]), signature) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		ComputeID string   `json:"compute_id"`
		Scope     string   `json:"scope"`
		Exp       int64    `json:"exp"`
		Iat       int64    `json:"iat"`
		Audience  []string `json:"aud"`
	}
	if json.Unmarshal(b, &claims) != nil || claims.ComputeID != workload || claims.Scope != "" || claims.Exp <= now.Unix() || claims.Exp > now.Add(90*time.Second).Unix() || claims.Iat > now.Add(30*time.Second).Unix() || claims.Iat <= 0 || claims.Iat < now.Add(-90*time.Second).Unix() || claims.Exp <= claims.Iat || claims.Exp-claims.Iat > 90 {
		return false
	}
	return len(claims.Audience) == 1 && claims.Audience[0] == "compute"
}
func (g *computeGateway) configure(w http.ResponseWriter, r *http.Request) {
	workload := r.PathValue("compute")
	if r.TLS == nil || r.TLS.ServerName != workload+"-management."+g.kube.namespace+".svc" {
		http.Error(w, "Endpoint identity mismatch", 403)
		return
	}
	secret, err := g.config(r.Context(), workload)
	if err != nil {
		http.Error(w, "Compute unavailable", 503)
		return
	}
	raw, err := secretText(secret, "config.json")
	if err != nil {
		http.Error(w, "Compute unavailable", 503)
		return
	}
	var stored map[string]any
	if json.Unmarshal([]byte(raw), &stored) != nil {
		http.Error(w, "Compute unavailable", 503)
		return
	}
	auth := strings.Fields(r.Header.Get("Authorization"))
	if len(auth) != 2 || !strings.EqualFold(auth[0], "Bearer") || !validComputeJWT(auth[1], workload, stored, time.Now()) {
		http.Error(w, "Invalid management credential", 401)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		http.Error(w, "Configuration exceeds limit", 413)
		return
	}
	var requested map[string]any
	if json.Unmarshal(body, &requested) != nil || !sameComputeIdentity(stored, requested) {
		http.Error(w, "Configuration identity mismatch", 403)
		return
	}
	service, err := g.kube.request(r.Context(), http.MethodGet, g.kube.path("service", workload+"-management"), nil)
	if err != nil || nested(service, "metadata", "labels", "neon-control/endpoint-id") != nested(secret, "metadata", "labels", "neon-control/endpoint-id") || nested(service, "metadata", "labels", "neon-control/project-id") != nested(secret, "metadata", "labels", "neon-control/project-id") {
		http.Error(w, "Management Service unavailable", 503)
		return
	}
	address := stringVal(nested(service, "spec", "clusterIP"))
	if net.ParseIP(address) == nil {
		http.Error(w, "Management Service unavailable", 503)
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://"+net.JoinHostPort(address, "3080")+"/configure", strings.NewReader(string(body)))
	if err != nil {
		http.Error(w, "Management request failed", 502)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+auth[1])
	response, err := g.http.Do(request)
	if err != nil {
		http.Error(w, "Native compute unavailable", 502)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		jsonResponse(w, response.StatusCode, record{"error": "Native compute configuration rejected"})
		return
	}
	result, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		http.Error(w, "Native compute response failed", 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(result)
}

func sameComputeIdentity(stored, requested map[string]any) bool {
	cluster, ok := nested(stored, "spec", "cluster", "cluster_id").(string)
	if !ok || cluster == "" || cluster != nested(requested, "spec", "cluster", "cluster_id") {
		return false
	}
	// Read-only compute mode is a tagged JSON object in the pinned native spec.
	// Comparing interfaces with != panics for maps supplied in a request. Treat
	// malformed or changed identities as a denial. The pinned ComputeSpec
	// defaults an omitted mode to Primary; preserve this native contract.
	mode, validMode := gatewayComputeMode(stored)
	incomingMode, validIncomingMode := gatewayComputeMode(requested)
	if !validMode || !validIncomingMode || !reflect.DeepEqual(mode, incomingMode) {
		return false
	}
	original, _ := json.Marshal(stored["compute_ctl_config"])
	incoming, _ := json.Marshal(requested["compute_ctl_config"])
	if string(original) != string(incoming) {
		return false
	}
	settings := func(config map[string]any) (map[string]string, bool) {
		values := map[string]string{}
		items, ok := nested(config, "spec", "cluster", "settings").([]any)
		if !ok {
			return nil, false
		}
		for _, item := range items {
			v, ok := item.(map[string]any)
			if !ok {
				return nil, false
			}
			name, nameOK := v["name"].(string)
			value, valueOK := v["value"].(string)
			if !nameOK || !valueOK || name == "" {
				return nil, false
			}
			if _, duplicate := values[name]; duplicate {
				// Do not rely on which duplicate setting the native parser uses.
				return nil, false
			}
			values[name] = value
		}
		return values, true
	}
	a, validA := settings(stored)
	b, validB := settings(requested)
	if !validA || !validB {
		return false
	}
	for _, key := range []string{"neon.tenant_id", "neon.timeline_id", "neon.pageserver_connstring"} {
		if a[key] == "" || a[key] != b[key] {
			return false
		}
	}
	// The native spec accepts top-level tenant/timeline/pageserver fields that
	// can override legacy GUCs. Freeze every non-catalog field, including future
	// unknown fields, rather than checking only the three legacy settings.
	identity := func(config map[string]any, canonicalMode any) map[string]any {
		root, spec, cluster := map[string]any{}, map[string]any{}, map[string]any{}
		for k, v := range config {
			root[k] = v
		}
		for k, v := range config["spec"].(map[string]any) {
			if k != "delta_operations" {
				spec[k] = v
			}
		}
		for k, v := range nested(config, "spec", "cluster").(map[string]any) {
			if k != "roles" && k != "databases" {
				cluster[k] = v
			}
		}
		spec["mode"], spec["cluster"], root["spec"] = canonicalMode, cluster, spec
		return root
	}
	return reflect.DeepEqual(identity(stored, mode), identity(requested, incomingMode))
}

func gatewayComputeMode(config map[string]any) (any, bool) {
	spec, ok := config["spec"].(map[string]any)
	if !ok {
		return nil, false
	}
	mode, present := spec["mode"]
	if !present {
		return "Primary", true
	}
	if text, ok := mode.(string); ok {
		return text, text == "Primary" || text == "Replica"
	}
	if tagged, ok := mode.(map[string]any); ok && len(tagged) == 1 {
		lsn, ok := tagged["Static"].(string)
		return mode, ok && lsn != ""
	}
	return nil, false
}
