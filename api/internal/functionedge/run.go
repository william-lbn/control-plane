package functionedge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"time"
)

func publicFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 64<<10 {
		return nil, errors.New("edge trust file unavailable")
	}
	return os.ReadFile(path)
}

func Run(ctx context.Context) error {
	cert, err := tls.LoadX509KeyPair(os.Getenv("NEON_FUNCTIONS_EDGE_CLIENT_CERT_FILE"), os.Getenv("NEON_FUNCTIONS_EDGE_CLIENT_KEY_FILE"))
	if err != nil {
		return errors.New("edge client identity unavailable")
	}
	ca, err := publicFile(os.Getenv("NEON_FUNCTIONS_EDGE_BROKER_CA_FILE"))
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return errors.New("edge broker CA rejected")
	}
	guestCA, err := publicFile(os.Getenv("NEON_FUNCTIONS_EDGE_GUEST_CA_FILE"))
	if err != nil {
		return err
	}
	handler, err := NewHandler(Config{PublicOrigin: os.Getenv("NEON_FUNCTIONS_PUBLIC_ORIGIN"), BrokerOrigin: os.Getenv("NEON_FUNCTIONS_EDGE_BROKER_ORIGIN"),
		BrokerTLS: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}}, GuestCA: guestCA, LabHTTP: os.Getenv("NEON_FUNCTIONS_PUBLIC_LAB_HTTP") == "true"})
	if err != nil {
		return err
	}
	bind := os.Getenv("NEON_FUNCTIONS_PUBLIC_BIND")
	if bind == "" {
		bind = ":8792"
	}
	server := &http.Server{Addr: bind, Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("function edge listener stopped")
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
		}
		return nil
	}
}
