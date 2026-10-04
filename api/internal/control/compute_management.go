package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Each Endpoint has an independent Ed25519 key. Only the public JWK enters
// compute_ctl's configuration; the seed never leaves its owned Secret.
func (k *kubeClient) computeControlKey(ctx context.Context, project, endpoint string) (ed25519.PrivateKey, error) {
	name := kubeName(endpoint) + "-credentials"
	for attempt := 0; attempt < 6; attempt++ {
		secret, err := k.request(ctx, http.MethodGet, k.path("secret", name), nil)
		if err != nil || !owned(secret, project, endpoint) {
			return nil, errors.New("compute control credential unavailable or unowned")
		}
		encoded := stringVal(nested(secret, "data", "controlSigningSeed"))
		if encoded != "" {
			seed, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil || len(seed) != ed25519.SeedSize {
				return nil, errors.New("invalid compute control signing seed")
			}
			return ed25519.NewKeyFromSeed(seed), nil
		}
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		secret["data"].(map[string]any)["controlSigningSeed"] = base64.StdEncoding.EncodeToString(key.Seed())
		_, err = k.request(ctx, http.MethodPut, k.path("secret", name), secret)
		var ke kubeError
		if errors.As(err, &ke) && ke.Status == 409 {
			continue
		}
		if err != nil {
			return nil, err
		}
		return key, nil
	}
	return nil, errors.New("compute control key reservation conflict")
}
func computeKeyID(key ed25519.PrivateKey) string {
	hash := sha256.Sum256(key.Public().(ed25519.PublicKey))
	return fmt.Sprintf("%x", hash[:16])
}
func installComputeJWK(config map[string]any, key ed25519.PrivateKey) {
	public := key.Public().(ed25519.PublicKey)
	config["compute_ctl_config"] = map[string]any{"jwks": map[string]any{"keys": []any{map[string]any{
		"use": "sig", "key_ops": []string{"verify"}, "alg": "EdDSA", "kid": computeKeyID(key),
		"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(public)}}}}
}
func computeJWT(key ed25519.PrivateKey, endpoint string, now time.Time) string {
	header, _ := json.Marshal(map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": computeKeyID(key)})
	// Deliberately omit the cluster-wide admin scope. The native server checks
	// compute_id against this exact workload, and exp limits replay duration.
	claims, _ := json.Marshal(map[string]any{"compute_id": kubeName(endpoint), "aud": []string{"compute"}, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
}

// The migration of old dummy JWKS requires a cold start, and is allowed only
// after the existing idle guard accepts the exact VM UID. Active workloads
// are rejected for operator retry; they are never forcefully restarted here.
func (s *server) ensureComputeManagement(ctx context.Context, p catalogPayload) (map[string]any, ed25519.PrivateKey, error) {
	key, err := s.kube.computeControlKey(ctx, p.ProjectID, p.EndpointID)
	if err != nil {
		return nil, nil, err
	}
	name := kubeName(p.EndpointID)
	tlsIdentity, err := s.kube.computeTLSIdentity(ctx, p.ProjectID, p.EndpointID, env("NEON_COMPUTE_CONTROL_HOST", s.proxyHost))
	if err != nil {
		return nil, nil, err
	}
	portChanged, err := s.managementPortTemplate(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	if vm, e := s.kube.request(ctx, http.MethodGet, s.kube.path("vm", name), nil); e == nil {
		if !owned(vm, p.ProjectID, p.EndpointID) {
			return nil, nil, errors.New("compute control VM ownership mismatch")
		}
		present := false
		ports, _ := nested(vm, "spec", "guest", "ports").([]any)
		for _, v := range ports {
			if number(v.(map[string]any)["port"]) == 3080 {
				present = true
			}
		}
		if !present {
			portChanged = true
		}
	} else {
		var ke kubeError
		if !errors.As(e, &ke) || ke.Status != 404 {
			return nil, nil, e
		}
	}
	for attempt := 0; attempt < 6; attempt++ {
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", name+"-config"), nil)
		if err != nil || !owned(secret, p.ProjectID, p.EndpointID) {
			return nil, nil, errors.New("compute control config missing or unowned")
		}
		raw, err := secretText(secret, "config.json")
		if err != nil {
			return nil, nil, err
		}
		var config map[string]any
		if err = json.Unmarshal([]byte(raw), &config); err != nil {
			return nil, nil, err
		}
		keys, _ := nested(config, "compute_ctl_config", "jwks", "keys").([]any)
		matches := len(keys) == 1 && stringVal(nested(keys[0].(map[string]any), "kid")) == computeKeyID(key)
		if !matches || portChanged || nested(config, "compute_ctl_config", "tls", "key_path") != "/run/neon-lab/control.key" || nested(secret, "data", "control.crt") != tlsIdentity["controlCert"] {
			_, err = s.kube.request(ctx, http.MethodGet, s.kube.path("vm", name), nil)
			var ke kubeError
			if err == nil {
				if err = s.suspendCompute(ctx, suspendPayload{ProjectID: p.ProjectID, EndpointID: p.EndpointID, WorkloadName: name}); err != nil {
					return nil, nil, err
				}
				s.logger.Info("compute control identity cold migration", "endpoint_id", p.EndpointID)
			} else if !errors.As(err, &ke) || ke.Status != 404 {
				return nil, nil, err
			}
			installComputeJWK(config, key)
			installComputeTLS(config)
			encoded, _ := json.Marshal(config)
			secret["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(encoded)
			secret["data"].(map[string]any)["control.crt"] = tlsIdentity["controlCert"]
			secret["data"].(map[string]any)["control.key"] = tlsIdentity["controlTLSKey"]
			_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("secret", name+"-config"), secret)
			if errors.As(err, &ke) && ke.Status == 409 {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
		}
		// Ensure the signed identity has been loaded by the guest's HTTP server.
		conn, err := s.catalogConnection(ctx, p)
		if err != nil {
			return nil, nil, err
		}
		conn.Close(context.Background())
		service := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": name + "-management", "labels": credentialLabels(p.ProjectID, p.EndpointID)},
			"spec": map[string]any{"type": "ClusterIP", "selector": map[string]string{"vm.neon.tech/name": name}, "ports": []any{map[string]any{"name": "compute-control", "port": 3080, "targetPort": 3080, "protocol": "TCP"}}}}
		if _, err = s.kube.createOwned(ctx, "service", service, p.ProjectID, p.EndpointID); err != nil {
			return nil, nil, err
		}
		// Earlier transport diagnostics used a NodePort. Native compute HTTP
		// must remain internal; only the authenticated TLS gateway is exposed.
		for n := 0; n < 6; n++ {
			existing, err := s.kube.request(ctx, http.MethodGet, s.kube.path("service", name+"-management"), nil)
			if err != nil || !owned(existing, p.ProjectID, p.EndpointID) {
				return nil, nil, errors.New("compute management Service ownership mismatch")
			}
			if nested(existing, "spec", "type") == "ClusterIP" {
				break
			}
			spec, ok := existing["spec"].(map[string]any)
			if !ok {
				return nil, nil, errors.New("invalid compute management Service")
			}
			spec["type"] = "ClusterIP"
			delete(spec, "externalTrafficPolicy")
			delete(spec, "healthCheckNodePort")
			ports, _ := spec["ports"].([]any)
			for _, item := range ports {
				port, ok := item.(map[string]any)
				if !ok {
					return nil, nil, errors.New("invalid compute management Service port")
				}
				delete(port, "nodePort")
			}
			_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("service", name+"-management"), existing)
			var conflict kubeError
			if errors.As(err, &conflict) && conflict.Status == 409 {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			break
		}
		return config, key, nil
	}
	return nil, nil, errors.New("compute control config update conflict")
}

func (s *server) managementPortTemplate(ctx context.Context, p catalogPayload) (bool, error) {
	for attempt := 0; attempt < 6; attempt++ {
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
		if err != nil {
			return false, err
		}
		raw, err := secretText(secret, "routes.json")
		if err != nil {
			return false, err
		}
		var routes map[string]map[string]any
		if err = json.Unmarshal([]byte(raw), &routes); err != nil {
			return false, err
		}
		route := routes[selector(p.EndpointID)]
		if route == nil || route["project_id"] != p.ProjectID || route["branch_id"] != p.BranchID {
			return false, errors.New("compute control route ownership mismatch")
		}
		guest, ok := nested(route, "vm_template", "spec", "guest").(map[string]any)
		if !ok {
			return false, errors.New("compute control template missing")
		}
		ports, _ := guest["ports"].([]any)
		changed := false
		present := false
		for _, v := range ports {
			if number(v.(map[string]any)["port"]) == 3080 {
				present = true
			}
		}
		if !present {
			guest["ports"] = append(ports, map[string]any{"name": "compute-control", "port": 3080})
			changed = true
		}
		disks := nested(route, "vm_template", "spec", "disks").([]any)
		for _, v := range disks {
			disk := v.(map[string]any)
			if disk["name"] != "compute-config" {
				continue
			}
			projection := disk["secret"].(map[string]any)
			items := projection["items"].([]any)
			for _, key := range []string{"control.crt", "control.key"} {
				found := false
				for _, item := range items {
					if item.(map[string]any)["key"] == key {
						found = true
					}
				}
				if !found {
					items = append(items, map[string]any{"key": key, "path": key})
					changed = true
				}
			}
			projection["items"] = items
		}
		if !changed {
			return false, nil
		}
		encoded, _ := json.Marshal(routes)
		secret["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(encoded)
		_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("secret", routesSecret), secret)
		var ke kubeError
		if errors.As(err, &ke) && ke.Status == 409 {
			continue
		}
		return err == nil, err
	}
	return false, errors.New("compute control template update conflict")
}

func (s *server) nativeCreateRole(ctx context.Context, p catalogPayload, verifier string) error {
	config, key, err := s.ensureComputeManagement(ctx, p)
	if err != nil {
		return err
	}
	conn, err := s.catalogConnection(ctx, p)
	if err != nil {
		return err
	}
	var count int
	var exists bool
	err = conn.QueryRow(ctx, "SELECT count(*),(SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)) FROM pg_roles WHERE rolcanlogin", p.Name).Scan(&count, &exists)
	conn.Close(context.Background())
	if err != nil {
		return err
	}
	if !exists && count >= 500 {
		return errors.New("branch login role quota exceeded")
	}
	if err = catalogDelta(config, p, "create_role", verifier); err != nil {
		return err
	}
	// PostgreSQL 16 requires ADMIN OPTION for subsequent service mutations.
	// Only CREATE accepts this option. It is sent on the first native CREATE,
	// not kept in cold specs or ALTER ROLE password configuration.
	for _, v := range nested(config, "spec", "cluster", "roles").([]any) {
		role := v.(map[string]any)
		if role["name"] == p.Name {
			role["options"] = []any{map[string]any{"name": "ADMIN", "value": "control_probe", "vartype": "enum"}}
		}
	}
	return s.nativeComputeRequest(ctx, p, config, key)
}
