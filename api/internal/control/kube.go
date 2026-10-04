package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type kubeClient struct {
	base, namespace, token, tokenFile string
	http                              *http.Client
}
type kubeError struct {
	Status  int
	Message string
}

func (e kubeError) Error() string { return fmt.Sprintf("kubernetes %d: %s", e.Status, e.Message) }

func newKubeClient() (*kubeClient, error) {
	namespace := env("NEON_KUBE_NAMESPACE", "neon")
	api := os.Getenv("NEON_KUBE_API")
	caFile, certFile, keyFile := os.Getenv("NEON_KUBE_CA"), os.Getenv("NEON_KUBE_CERT"), os.Getenv("NEON_KUBE_KEY")
	token := ""
	tokenFile := ""
	if api == "" {
		host := os.Getenv("KUBERNETES_SERVICE_HOST")
		if host == "" {
			return nil, errors.New("NEON_KUBE_API or Kubernetes service account required")
		}
		api = "https://" + host + ":" + env("KUBERNETES_SERVICE_PORT_HTTPS", "443")
		caFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
		tokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, err
		}
		token = strings.TrimSpace(string(b))
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid Kubernetes CA")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	if certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	return &kubeClient{base: strings.TrimRight(api, "/"), namespace: namespace, token: token, tokenFile: tokenFile,
		http: &http.Client{Timeout: 35 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil}}}, nil
}

func (k *kubeClient) request(ctx context.Context, method, path string, body any) (map[string]any, error) {
	return k.requestBearer(ctx, method, path, body, "")
}

func (k *kubeClient) requestBearer(ctx context.Context, method, path string, body any, bearer string) (map[string]any, error) {
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		input = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, input)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		contentType := "application/json"
		if method == http.MethodPatch {
			contentType = "application/merge-patch+json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	// Projected service-account tokens rotate. Re-read on each request so a
	// long-running gateway never keeps using its expired startup credential.
	token := k.token
	if k.tokenFile != "" {
		b, err := os.ReadFile(k.tokenFile)
		if err != nil {
			return nil, errors.New("Kubernetes service-account credential unavailable")
		}
		token = strings.TrimSpace(string(b))
		if token == "" {
			return nil, errors.New("empty Kubernetes service-account credential")
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		var detail struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &detail)
		if detail.Message == "" {
			detail.Message = resp.Status
		}
		return nil, kubeError{resp.StatusCode, detail.Message}
	}
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (k *kubeClient) path(kind, name string) string {
	n := url.PathEscape(k.namespace)
	var collection string
	switch kind {
	case "vm":
		collection = "/apis/vm.neon.tech/v1/namespaces/" + n + "/virtualmachines"
	case "deployment":
		collection = "/apis/apps/v1/namespaces/" + n + "/deployments"
	case "pod":
		collection = "/api/v1/namespaces/" + n + "/pods"
	case "secret":
		collection = "/api/v1/namespaces/" + n + "/secrets"
	case "service":
		collection = "/api/v1/namespaces/" + n + "/services"
	default:
		panic("unknown Kubernetes kind")
	}
	if name != "" {
		collection += "/" + url.PathEscape(name)
	}
	return collection
}

func (k *kubeClient) serviceRequest(ctx context.Context, service string, port int, path, method string, body any) (map[string]any, error) {
	proxyPath := fmt.Sprintf("/api/v1/namespaces/%s/services/http:%s:%d/proxy/%s",
		url.PathEscape(k.namespace), url.PathEscape(service), port, strings.TrimLeft(path, "/"))
	return k.request(ctx, method, proxyPath, body)
}

func nested(item map[string]any, names ...string) any {
	var value any = item
	for _, name := range names {
		m, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = m[name]
	}
	return value
}
func number(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		n, _ := strconv.ParseFloat(v, 64)
		return n
	}
	return 0
}
func stringVal(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func cpuMilli(value any) int {
	raw := stringVal(value)
	if strings.HasSuffix(raw, "m") {
		n, _ := strconv.ParseFloat(strings.TrimSuffix(raw, "m"), 64)
		return int(n)
	}
	n, _ := strconv.ParseFloat(raw, 64)
	return int(n * 1000)
}

func (k *kubeClient) runtime(ctx context.Context, kind, name string) map[string]any {
	result := map[string]any{"observed_state": "unknown", "observed_at": time.Now().UTC().Format(time.RFC3339Nano)}
	apiKind := kind
	if kind == "neonvm" {
		apiKind = "vm"
	}
	if apiKind != "vm" && apiKind != "deployment" {
		result["error"] = "unsupported_workload"
		return result
	}
	item, err := k.request(ctx, "GET", k.path(apiKind, name), nil)
	if err != nil {
		var ke kubeError
		if errors.As(err, &ke) && ke.Status == 404 && apiKind == "vm" {
			result["observed_state"] = "suspended"
			result["phase"] = "Absent"
			result["cpu_milli"] = 0
			result["memory_mib"] = 0
		} else {
			result["error"] = "kubernetes_unavailable"
		}
		return result
	}
	if kind == "deployment" {
		replicas := int(number(nested(item, "spec", "replicas")))
		ready := int(number(nested(item, "status", "readyReplicas")))
		result["replicas"], result["ready_replicas"] = replicas, ready
		if replicas == 0 {
			result["observed_state"] = "suspended"
		} else if ready > 0 {
			result["observed_state"] = "active"
		} else {
			result["observed_state"] = "starting"
		}
		return result
	}
	phase := stringVal(nested(item, "status", "phase"))
	result["phase"] = phase
	result["workload_uid"] = nested(item, "metadata", "uid")
	result["workload_created_at"] = nested(item, "metadata", "creationTimestamp")
	if phase == "Running" || phase == "Scaling" {
		result["observed_state"] = "active"
	} else {
		result["observed_state"] = "starting"
	}
	result["cpu_milli"] = cpuMilli(nested(item, "spec", "guest", "cpus", "use"))
	result["memory_mib"] = int(number(nested(item, "spec", "guest", "memorySlots", "use"))) * 1024
	result["pod_name"] = nested(item, "status", "podName")
	result["node_name"] = nested(item, "status", "nodeName")
	return result
}
