package control

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/william-lbn/control-plane/api/internal/adapter"
)

type adapterKubernetes struct{ kube *kubeClient }

func (a adapterKubernetes) Request(ctx context.Context, method, path string, body any) (map[string]any, error) {
	result, err := a.kube.request(ctx, method, path, body)
	// Preserve the old bounded retry only for transient reads. Neither malformed
	// JSON, HTTP failures, TLS trust errors, cancellation nor mutations are replayed.
	if err != nil && method == http.MethodGet && body == nil && ctx.Err() == nil && transientAdapterRead(err) {
		if pauseErr := waitAdapterRetry(ctx); pauseErr == nil {
			result, err = a.kube.request(ctx, method, path, body)
		}
	}
	var dependency kubeError
	if errors.As(err, &dependency) {
		return nil, adapter.APIError{Status: dependency.Status}
	}
	return result, err
}
func transientAdapterRead(err error) bool {
	var trust x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var host x509.HostnameError
	if errors.As(err, &trust) || errors.As(err, &invalid) || errors.As(err, &host) {
		return false
	}
	var network net.Error
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ETIMEDOUT) || (errors.As(err, &network) && network.Timeout())
}
func waitAdapterRetry(ctx context.Context) error {
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func RunProxyAdapter(ctx context.Context) error {
	kube, err := newKubeClient()
	if err != nil {
		return err
	}
	kube.namespace = env("POD_NAMESPACE", kube.namespace)
	kube.http.Timeout = 15 * time.Second
	wakes, err := strconv.Atoi(env("NEON_ADAPTER_MAX_WAKES", "8"))
	if err != nil {
		return errors.New("invalid wake concurrency")
	}
	timeout, err := time.ParseDuration(env("NEON_ADAPTER_WAKE_TIMEOUT", "300s"))
	if err != nil {
		return errors.New("invalid wake timeout")
	}
	node, err := strconv.ParseUint(env("NEON_ADAPTER_PAGESERVER_NODE_ID", "2"), 10, 64)
	if err != nil {
		return errors.New("invalid Pageserver node ID")
	}
	config := adapter.Config{Namespace: kube.namespace, RoutesSecret: env("ROUTES_SECRET", routesSecret), NotificationsConfigMap: env("NOTIFICATIONS_CONFIGMAP", "neon-control-notifications"),
		ProxyToken: adapter.Credential{File: os.Getenv("PROXY_API_TOKEN_FILE"), Value: os.Getenv("PROXY_API_TOKEN")}, HookToken: adapter.Credential{File: os.Getenv("CONTROLLER_HOOK_TOKEN_FILE"), Value: os.Getenv("CONTROLLER_HOOK_TOKEN")},
		WakeTimeout: timeout, PollInterval: 2 * time.Second, MaxConcurrentWakes: wakes, PageserverNodeID: node}
	service, err := adapter.New(config, adapterKubernetes{kube}, slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: env("NEON_ADAPTER_BIND", ":8080"), Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: timeout + 10*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	result := make(chan error, 1)
	go func() { result <- httpServer.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-result:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if stopErr := httpServer.Shutdown(shutdown); stopErr != nil {
		_ = httpServer.Close()
		if err == nil {
			err = stopErr
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
