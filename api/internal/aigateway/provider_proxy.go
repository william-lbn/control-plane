package aigateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

// Operator-approved LAN CONNECT proxy; environment proxies are never consulted.
// Requests to private provider addresses always stay direct.
func (p *ProviderNetworkPolicy) WithEgressProxy(value string) (*ProviderNetworkPolicy, error) {
	if p == nil || p.lookup == nil {
		return nil, errProviderEndpoint
	}
	clone := *p
	if value == "" {
		clone.egressProxy = ""
		return &clone, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Path != "" && u.Path != "/" {
		return nil, errProviderEndpoint
	}
	ip, err := netip.ParseAddr(u.Hostname())
	port, e := strconv.Atoi(u.Port())
	if err != nil || e != nil || port < 1 || port > 65535 || providerAddressAlwaysDenied(ip) || !(ip.IsPrivate() || ip.IsLoopback()) {
		return nil, errProviderEndpoint
	}
	clone.egressProxy = "http://" + net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(port))
	return &clone, nil
}
func (p *ProviderNetworkPolicy) ConfigureRoute(endpoint ResolvedProviderEndpoint, route Route, secret string) (Route, error) {
	if p.ValidateResolved(endpoint) != nil {
		return Route{}, errProviderEndpoint
	}
	route.BaseURL = endpoint.BaseURL
	route.AllowedIPs = append([]string(nil), endpoint.AllowedIPs...)
	route.LabHTTP = endpoint.LabHTTP
	route.egressProxy = ""
	public := !endpoint.LabHTTP
	for _, raw := range endpoint.AllowedIPs {
		ip, err := netip.ParseAddr(raw)
		if err != nil || !providerPublicAddress(ip) {
			public = false
		}
	}
	if public {
		route.egressProxy = p.egressProxy
	}
	return route.WithProviderCredential(secret)
}

type providerProxyReader struct {
	reader    io.Reader
	remaining int64
}

func (r *providerProxyReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, errors.New("proxy response headers exceed limit")
	}
	if r.remaining > 0 && int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	if r.remaining > 0 {
		r.remaining -= int64(n)
	}
	return n, err
}

type providerProxyConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *providerProxyConnection) Read(p []byte) (int, error) { return c.reader.Read(p) }
func dialProviderProxy(ctx context.Context, network, proxyAddress, targetAddress string) (net.Conn, error) {
	proxy, err := url.Parse(proxyAddress)
	if err != nil {
		return nil, errProviderEndpoint
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, proxy.Host)
	if err != nil {
		return nil, err
	}
	fail := func() (net.Conn, error) { conn.Close(); return nil, errors.New("approved provider egress unavailable") }
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if conn.SetDeadline(deadline) != nil {
		return fail()
	}
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: targetAddress}, Host: targetAddress, Header: make(http.Header)}
	if request.Write(conn) != nil {
		return fail()
	}
	limited := &providerProxyReader{reader: conn, remaining: 32768}
	reader := bufio.NewReader(limited)
	response, err := http.ReadResponse(reader, request)
	if err != nil || response.StatusCode != 200 {
		return fail()
	}
	limited.remaining = -1
	if conn.SetDeadline(time.Time{}) != nil {
		return fail()
	}
	// CONNECT uses the validated IP literal, never proxy-side provider DNS.
	// The outer HTTPS transport still verifies the original provider hostname.
	return &providerProxyConnection{Conn: conn, reader: reader}, nil
}
