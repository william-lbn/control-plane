package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/william-lbn/control-plane/api/internal/dataapi"
)

func main() {
	printJWKS := flag.Bool("print-delegation-jwks", false, "Print the public verification key only, then exit")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	configFile, err := os.Open(os.Getenv("NEON_DATA_API_CONFIG_FILE"))
	if err != nil {
		logger.Error("data API configuration unavailable")
		os.Exit(1)
	}
	config, err := dataapi.ParseConfig(configFile)
	configFile.Close()
	if err != nil {
		logger.Error("data API configuration rejected", "error", err)
		os.Exit(1)
	}
	encoded, err := os.ReadFile(os.Getenv("NEON_DATA_API_SIGNING_SEED_FILE"))
	if err != nil {
		logger.Error("delegation signing key unavailable")
		os.Exit(1)
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		logger.Error("delegation signing key rejected")
		os.Exit(1)
	}
	if config.AllowPlaintextUpstream && os.Getenv("NEON_DATA_API_ALLOW_PLAINTEXT_UPSTREAM") != "true" {
		logger.Error("plaintext upstream requires an explicit laboratory profile")
		os.Exit(1)
	}
	var ca []byte
	if path := os.Getenv("NEON_DATA_API_UPSTREAM_CA_FILE"); path != "" {
		ca, err = os.ReadFile(path)
		if err != nil {
			logger.Error("upstream CA unavailable")
			os.Exit(1)
		}
	}
	gateway, err := dataapi.NewWithTrust(config, seed, logger, ca)
	if err != nil {
		logger.Error("data API gateway initialization rejected", "error", err)
		os.Exit(1)
	}
	if *printJWKS {
		fmt.Println(string(gateway.DelegationJWKS()))
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	bind := os.Getenv("NEON_DATA_API_BIND")
	if bind == "" {
		bind = ":9080"
	}
	healthBind := os.Getenv("NEON_DATA_API_HEALTH_BIND")
	if healthBind == "" {
		healthBind = ":9081"
	}
	server := &http.Server{Addr: bind, Handler: gateway, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 160 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32768}
	health := &http.Server{Addr: healthBind, Handler: gateway.HealthHandler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 4096}
	failures := make(chan error, 2)
	go func() { failures <- server.ListenAndServe() }()
	go func() { failures <- health.ListenAndServe() }()
	failed := false
	select {
	case <-ctx.Done():
	case <-failures:
		failed = true
		logger.Error("data API listener stopped")
	}
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	server.Shutdown(shutdown)
	health.Shutdown(shutdown)
	if failed {
		os.Exit(1)
	}
}
