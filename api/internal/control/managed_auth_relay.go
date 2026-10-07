package control

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

var managedAuthRelaySlots = make(chan struct{}, 32)
var managedAuthActions = map[string]bool{"sign-up/email": true, "sign-in/email": true, "sign-out": true, "get-session": true, "token": true, "jwks": true, "list-sessions": true, "revoke-session": true, "revoke-sessions": true, "revoke-other-sessions": true, "change-password": true, "update-user": true}

// This application-identity boundary deliberately excludes Console cookies,
// authorization/CSRF headers and caller-controlled forwarding headers. Only a
// fixed, live, owned branch runtime can be selected; deletion closes admission
// in metadata before the worker terminates any workload.
func (s *server) managedAuthRelay(w http.ResponseWriter, r *http.Request) {
	branch := r.PathValue("authBranch")
	if !managedAuthEnabled() || !dataBranchID.MatchString(branch) || !managedAuthActions[r.PathValue("rest")] || (r.Method != "GET" && r.Method != "POST" && r.Method != "OPTIONS") {
		http.NotFound(w, r)
		return
	}
	select {
	case managedAuthRelaySlots <- struct{}{}:
		defer func() { <-managedAuthRelaySlots }()
	default:
		w.WriteHeader(429)
		return
	}
	var endpoint string
	if err := s.db.QueryRow(r.Context(), `SELECT a.endpoint_id FROM managed_auth_instances a JOIN branches b ON b.id=a.branch_id JOIN projects p ON p.id=a.project_id WHERE a.branch_id=$1 AND a.state='active' AND b.state='ready' AND b.deleted_at IS NULL AND p.state='ready' AND p.deleted_at IS NULL`, branch).Scan(&endpoint); err != nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	target, _ := url.Parse("http://" + managedAuthName(branch) + "." + s.kube.namespace + ".svc.cluster.local:9082")
	base, _ := url.Parse(managedAuthBase(branch))
	cookieName := "neon_app_" + branch + ".session_token"
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		pr.Out.Host = base.Host
		for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-CSRF-Token", "X-Real-IP", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Neon-Client-IP"} {
			pr.Out.Header.Del(h)
		}
		for _, cookie := range pr.In.Cookies() {
			if cookie.Name == cookieName || cookie.Name == "__Secure-"+cookieName {
				pr.Out.AddCookie(cookie)
			}
		}
		host, _, _ := net.SplitHostPort(pr.In.RemoteAddr)
		if host != "" {
			pr.Out.Header.Set("X-Neon-Client-IP", host)
		}
		pr.Out.Header.Set("X-Forwarded-Host", base.Host)
		pr.Out.Header.Set("X-Forwarded-Proto", base.Scheme)
	}, Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 130 * time.Second, MaxConnsPerHost: 8, DisableKeepAlives: true},
		ModifyResponse: func(response *http.Response) error {
			cookies := response.Cookies()
			response.Header.Del("Set-Cookie")
			for _, c := range cookies {
				if (c.Name == cookieName || c.Name == "__Secure-"+cookieName) && c.Domain == "" && c.Path == "/auth/v1/"+branch && c.HttpOnly {
					response.Header.Add("Set-Cookie", c.String())
				}
			}
			response.Header.Del("Set-Auth-JWT") // Explicit /token retrieves a token; session reads never expose it in headers.
			response.Header.Set("Cache-Control", "no-store")
			return nil
		}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			fail(w, r, 503, "auth_unavailable", "Managed Auth runtime unavailable")
		}}
	release, err := s.acquireProbeGate(r.Context(), endpoint, true)
	if err != nil {
		fail(w, r, 503, "auth_unavailable", "Could not admit Auth request")
		return
	}
	defer release()
	// Queries belong to the bounded library API, never to an arbitrary target URL.
	if strings.Contains(r.URL.RawQuery, "callbackURL=") {
		fail(w, r, 422, "invalid_request", "Use an exact trusted origin in the Auth body")
		return
	}
	proxy.ServeHTTP(w, r)
}
