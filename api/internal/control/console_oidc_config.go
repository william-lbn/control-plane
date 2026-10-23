package control

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type consoleOIDCProvider struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Organization     string   `json:"organization_id"`
	Issuer           string   `json:"issuer"`
	AuthorizationURL string   `json:"authorization_url"`
	TokenURL         string   `json:"token_url"`
	JWKSURL          string   `json:"jwks_url"`
	ClientID         string   `json:"client_id"`
	ClientSecret     string   `json:"client_secret"`
	AuthMethod       string   `json:"auth_method"`
	AllowedIPs       []string `json:"allowed_ips"`
	LabHTTP          bool     `json:"lab_http"`
	revision         string
	client           *http.Client
	verifier         *oidc.IDTokenVerifier
}
type consoleOIDCSettings struct {
	Version      int                    `json:"version"`
	PublicOrigin string                 `json:"public_origin"`
	LabHTTP      bool                   `json:"lab_http"`
	EgressProxy  string                 `json:"egress_proxy_url"`
	Providers    []*consoleOIDCProvider `json:"providers"`
	byID         map[string]*consoleOIDCProvider
}

func (*consoleOIDCSettings) String() string     { return "[Console OIDC settings redacted]" }
func (*consoleOIDCProvider) String() string     { return "[Console OIDC provider redacted]" }
func (p *consoleOIDCSettings) GoString() string { return p.String() }
func (p *consoleOIDCProvider) GoString() string { return p.String() }

var errConsoleOIDCConfig = errors.New("approved Console OIDC configuration required")

// URLs and exact addresses are operator-provisioned, never obtained from a
// browser or an untrusted discovery document. No redirects or environment proxy.
func consoleOIDCURL(raw string, lab bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || strings.TrimSpace(raw) != raw || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(u.Host, "\\%") || (u.Scheme != "https" && !(lab && u.Scheme == "http")) {
		return nil, errConsoleOIDCConfig
	}
	return u, nil
}

type consoleOIDCBoundedTransport struct {
	next    http.RoundTripper
	allowed map[string]bool
}

func (t consoleOIDCBoundedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !t.allowed[r.URL.String()] || (r.Method != http.MethodGet && r.Method != http.MethodPost) {
		return nil, errConsoleOIDCConfig
	}
	response, err := t.next.RoundTrip(r)
	if err == nil {
		response.Body = &consoleOIDCBoundedBody{ReadCloser: response.Body, remaining: 1 << 20}
	}
	return response, err
}

type consoleOIDCBoundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *consoleOIDCBoundedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, errors.New("OIDC response exceeds limit")
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func newConsoleOIDCSettings(raw []byte, secure bool) (*consoleOIDCSettings, error) {
	var c consoleOIDCSettings
	if aiProviderBoundedJSON(raw, &c) != nil || c.Version != 1 || len(c.Providers) < 1 || len(c.Providers) > 16 {
		return nil, errConsoleOIDCConfig
	}
	origin, err := consoleOIDCURL(c.PublicOrigin, c.LabHTTP)
	if err != nil || origin.Path != "" || (origin.Scheme == "https" && !secure) || (origin.Scheme == "http" && secure) {
		return nil, errConsoleOIDCConfig
	}
	var proxy *url.URL
	if c.EgressProxy != "" {
		proxy, err = url.Parse(c.EgressProxy)
		if err != nil || proxy.Scheme != "http" || proxy.Host == "" || proxy.User != nil || proxy.Path != "" || proxy.RawQuery != "" || proxy.Fragment != "" {
			return nil, errConsoleOIDCConfig
		}
	}
	c.byID = map[string]*consoleOIDCProvider{}
	for _, p := range c.Providers {
		if p == nil || !aiProviderSourceID.MatchString(p.ID) || !displayName(p.Name) || !aiProviderUserID.MatchString(p.Organization) || p.ClientID == "" || len(p.ClientID) > 255 || len(p.ClientSecret) < 16 || len(p.ClientSecret) > 4096 || strings.ContainsAny(p.ClientID+p.ClientSecret, "\r\n\x00") || c.byID[p.ID] != nil || (p.AuthMethod != "client_secret_basic" && p.AuthMethod != "client_secret_post") || len(p.AllowedIPs) < 1 || len(p.AllowedIPs) > 8 {
			return nil, errConsoleOIDCConfig
		}
		issuer, e := consoleOIDCURL(p.Issuer, p.LabHTTP)
		if e != nil || p.LabHTTP && !c.LabHTTP {
			return nil, errConsoleOIDCConfig
		}
		allowed := map[string]bool{}
		for _, rawURL := range []string{p.AuthorizationURL, p.TokenURL, p.JWKSURL} {
			u, e := consoleOIDCURL(rawURL, p.LabHTTP)
			if e != nil || u.Scheme != issuer.Scheme || u.Host != issuer.Host {
				return nil, errConsoleOIDCConfig
			}
			allowed[rawURL] = true
		}
		pins := map[string]bool{}
		private := false
		public := false
		for _, rawIP := range p.AllowedIPs {
			ip, e := netip.ParseAddr(rawIP)
			if e != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				return nil, errConsoleOIDCConfig
			}
			ip = ip.Unmap()
			pins[ip.String()] = true
			if ip.IsPrivate() || ip.IsLoopback() {
				private = true
			} else {
				public = true
			}
		}
		if private && public || issuer.Scheme == "http" && !private {
			return nil, errConsoleOIDCConfig
		}
		transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 32 << 10, DisableCompression: true}
		// Every DNS answer must retain its approved pin. The actual dial uses
		// the checked literal, avoiding a second DNS lookup/rebind.
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, e := net.SplitHostPort(address)
			if e != nil {
				return nil, e
			}
			ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if e != nil || len(ips) == 0 || len(ips) > 8 {
				return nil, errConsoleOIDCConfig
			}
			for _, ip := range ips {
				if !pins[ip.Unmap().String()] {
					return nil, errConsoleOIDCConfig
				}
			}
			target := net.JoinHostPort(ips[0].Unmap().String(), port)
			if proxy != nil && !private {
				return consoleOIDCConnect(ctx, proxy, target)
			}
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, target)
		}
		// Only WAN endpoints use the configured CONNECT proxy. TLS remains
		// end-to-end, using the original hostname and system trust roots.
		p.client = &http.Client{Transport: consoleOIDCBoundedTransport{transport, allowed}, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		// A client-secret rotation does not change the external identity or
		// resurrect revoked bindings. Changes to issuer/client/endpoints/pins
		// require an explicit organization policy update and identity binding.
		identity, _ := json.Marshal([]any{p.ID, p.Organization, p.Issuer, p.AuthorizationURL, p.TokenURL, p.JWKSURL, p.ClientID, p.AuthMethod, p.AllowedIPs, p.LabHTTP})
		p.revision = digest(string(identity))
		c.byID[p.ID] = p
		keyContext := oidc.ClientContext(context.Background(), p.client)
		p.verifier = oidc.NewVerifier(p.Issuer, oidc.NewRemoteKeySet(keyContext, p.JWKSURL), &oidc.Config{ClientID: p.ClientID, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}})
	}
	return &c, nil
}

func consoleOIDCConnect(ctx context.Context, proxy *url.URL, target string) (net.Conn, error) {
	address := proxy.Host
	if proxy.Port() == "" {
		address = net.JoinHostPort(proxy.Hostname(), "80")
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(8 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	if _, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReaderSize(conn, 32<<10)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 || reader.Buffered() != 0 {
		conn.Close()
		return nil, errConsoleOIDCConfig
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func loadConsoleOIDCSettings(secure bool) (*consoleOIDCSettings, error) {
	path := os.Getenv("NEON_CONSOLE_OIDC_CONFIG_FILE")
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errConsoleOIDCConfig
	}
	return newConsoleOIDCSettings(raw, secure)
}
func (p *consoleOIDCProvider) oauth(c *consoleOIDCSettings) *oauth2.Config {
	style := oauth2.AuthStyleInHeader
	if p.AuthMethod == "client_secret_post" {
		style = oauth2.AuthStyleInParams
	}
	return &oauth2.Config{ClientID: p.ClientID, ClientSecret: p.ClientSecret, Endpoint: oauth2.Endpoint{AuthURL: p.AuthorizationURL, TokenURL: p.TokenURL, AuthStyle: style}, RedirectURL: c.PublicOrigin + "/auth/sso/callback", Scopes: []string{oidc.ScopeOpenID}}
}
