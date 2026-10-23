package control

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var managedAuthRelaySlots = make(chan struct{}, 32)
var managedAuthActions = map[string]bool{"sign-up/email": true, "sign-in/email": true, "sign-out": true, "get-session": true, "token": true, "jwks": true, "list-sessions": true, "revoke-session": true, "revoke-sessions": true, "revoke-other-sessions": true, "change-password": true, "update-user": true}
var managedAuthOTPActions = map[string]bool{"email-otp/send-verification-otp": true, "email-otp/check-verification-otp": true, "email-otp/verify-email": true, "email-otp/request-password-reset": true, "email-otp/reset-password": true, "sign-in/email-otp": true}
var managedAuthTwoFactorActions = map[string]bool{"two-factor/enable": true, "two-factor/disable": true, "two-factor/get-totp-uri": true, "two-factor/verify-totp": true, "two-factor/verify-backup-code": true, "two-factor/generate-backup-codes": true}
var managedAuthAdminActions = map[string]string{
	"admin/get-user": "GET", "admin/list-users": "GET",
	"admin/create-user": "POST", "admin/update-user": "POST", "admin/set-role": "POST",
	"admin/ban-user": "POST", "admin/unban-user": "POST", "admin/list-user-sessions": "POST",
	"admin/revoke-user-session": "POST", "admin/revoke-user-sessions": "POST", "admin/set-user-password": "POST",
	"admin/remove-user": "POST", "admin/impersonate-user": "POST", "admin/stop-impersonating": "POST", "admin/has-permission": "POST",
}
var managedAuthResetCallback = regexp.MustCompile(`^reset-password/[a-zA-Z0-9_-]{20,128}$`)

func managedAuthRelayCookieAllowed(name, branch string, twoFactor bool, admin ...bool) bool {
	base := strings.TrimPrefix(name, "__Secure-")
	prefix := "neon_app_" + branch + "."
	return base == prefix+"session_token" || (twoFactor && base == prefix+"two_factor") || (len(admin) == 1 && admin[0] && base == prefix+"admin_session")
}

func managedAuthAllowedAction(action, method string) bool {
	if required, ok := managedAuthAdminActions[action]; ok {
		return method == required || method == "OPTIONS"
	}
	if managedAuthActions[action] {
		return true
	}
	if managedAuthTwoFactorActions[action] {
		return method == "POST" || method == "OPTIONS"
	}
	if !managedAuthMailConfigured() {
		return false
	}
	if action == "sign-in/magic-link" {
		return method == "POST" || method == "OPTIONS"
	}
	if action == "magic-link/verify" {
		return method == "GET" || method == "OPTIONS"
	}
	if managedAuthOTPActions[action] {
		return method == "POST" || method == "OPTIONS"
	}
	if managedAuthResetCallback.MatchString(action) {
		return method == "GET"
	}
	switch action {
	case "request-password-reset", "reset-password", "send-verification-email":
		return method == "POST" || method == "OPTIONS"
	case "verify-email":
		return method == "GET" || method == "OPTIONS"
	}
	return false
}

func managedAuthCallbackTrusted(value string, spec ManagedAuthSpec, publicOrigin string) bool {
	if len(value) > 2048 {
		return false
	}
	target, err := url.Parse(value)
	if err != nil || target.User != nil || target.Opaque != "" || target.Host == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return false
	}
	origin := target.Scheme + "://" + target.Host
	if !validAuthOrigin(origin) {
		return false
	}
	if origin == publicOrigin {
		return true
	}
	for _, trusted := range spec.AllowedOrigins {
		if origin == trusted {
			return true
		}
	}
	return false
}

// Preflight does not contact the branch database or acquire a SQL admission.
// Credentials require an exact configured origin; reflection and wildcards
// would expose application sessions to unrelated browser applications.
func managedAuthCORS(w http.ResponseWriter, r *http.Request, spec ManagedAuthSpec, publicOrigin string) bool {
	w.Header().Add("Vary", "Origin")
	origin := r.Header.Get("Origin")
	allowed := origin == publicOrigin
	for _, trusted := range spec.AllowedOrigins {
		allowed = allowed || origin == trusted
	}
	if (origin != "" && !allowed) || (r.Method == "OPTIONS" && origin == "") {
		fail(w, r, 403, "auth_origin_denied", "Origin is not trusted by this branch")
		return true
	}
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	if r.Method != "OPTIONS" {
		return false
	}
	method := r.Header.Get("Access-Control-Request-Method")
	if method != "GET" && method != "POST" {
		fail(w, r, 403, "auth_origin_denied", "Unsupported application method")
		return true
	}
	for _, header := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		if name := strings.TrimSpace(header); name != "" && !strings.EqualFold(name, "content-type") {
			fail(w, r, 403, "auth_origin_denied", "Unsupported application request header")
			return true
		}
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "300")
	w.WriteHeader(http.StatusNoContent)
	return true
}

// This application-identity boundary deliberately excludes Console cookies,
// authorization/CSRF headers and caller-controlled forwarding headers. Only a
// fixed, live, owned branch runtime can be selected; deletion closes admission
// in metadata before the worker terminates any workload.
func (s *server) managedAuthRelay(w http.ResponseWriter, r *http.Request) {
	branch := r.PathValue("authBranch")
	if !managedAuthEnabled() || !dataBranchID.MatchString(branch) || !managedAuthAllowedAction(r.PathValue("rest"), r.Method) || (r.Method != "GET" && r.Method != "POST" && r.Method != "OPTIONS") {
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
	var rawSpec []byte
	if err := s.db.QueryRow(r.Context(), `SELECT a.endpoint_id,a.spec FROM managed_auth_instances a JOIN branches b ON b.id=a.branch_id JOIN projects p ON p.id=a.project_id WHERE a.branch_id=$1 AND a.state='active' AND b.state='ready' AND b.deleted_at IS NULL AND p.state='ready' AND p.deleted_at IS NULL`, branch).Scan(&endpoint, &rawSpec); err != nil {
		http.NotFound(w, r)
		return
	}
	var spec ManagedAuthSpec
	if json.Unmarshal(rawSpec, &spec) != nil {
		fail(w, r, 503, "auth_unavailable", "Branch Auth configuration unavailable")
		return
	}
	if _, adminAction := managedAuthAdminActions[r.PathValue("rest")]; adminAction && !spec.AdminEnabled {
		http.NotFound(w, r)
		return
	}
	if managedAuthTwoFactorActions[r.PathValue("rest")] && !spec.TwoFactorEnabled {
		http.NotFound(w, r)
		return
	}
	if managedAuthOTPActions[r.PathValue("rest")] && !spec.EmailOTPEnabled {
		http.NotFound(w, r)
		return
	}
	if (r.PathValue("rest") == "sign-in/magic-link" || r.PathValue("rest") == "magic-link/verify") && !spec.MagicLinkEnabled {
		http.NotFound(w, r)
		return
	}
	if managedAuthCORS(w, r, spec, os.Getenv("NEON_AUTH_PUBLIC_ORIGIN")) {
		return
	}
	// Email links require browser redirects. Only the exact public/trusted
	// origins are permitted, before SQL admission or any Compute wake.
	for _, key := range []string{"callbackURL", "newUserCallbackURL", "errorCallbackURL"} {
		if callbacks, exists := r.URL.Query()[key]; exists {
			action := r.PathValue("rest")
			allowed := action == "magic-link/verify" || (key == "callbackURL" && (action == "verify-email" || managedAuthResetCallback.MatchString(action)))
			if len(callbacks) != 1 || !allowed || !managedAuthCallbackTrusted(callbacks[0], spec, os.Getenv("NEON_AUTH_PUBLIC_ORIGIN")) {
				fail(w, r, 422, "invalid_request", "Auth callback origin is not trusted")
				return
			}
		}
	}

	if managedAuthTwoFactorActions[r.PathValue("rest")] && r.Method == http.MethodPost {
		body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 16384))
		var input map[string]any
		if e != nil || json.Unmarshal(body, &input) != nil || input == nil {
			fail(w, r, 422, "invalid_factor_request", "Bounded factor JSON required")
			return
		}
		if value, exists := input["trustDevice"]; exists && value != false {
			fail(w, r, 422, "trusted_device_unavailable", "Trusted device memory is not enabled by this branch relay")
			return
		}
		if r.PathValue("rest") == "two-factor/enable" {
			if method, exists := input["method"]; exists && method != "totp" {
				fail(w, r, 422, "factor_method_unavailable", "This branch extension provides TOTP enrollment")
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	target, _ := url.Parse("http://" + managedAuthName(branch) + "." + s.kube.namespace + ".svc.cluster.local:9082")
	base, _ := url.Parse(managedAuthBase(branch))
	cookieAllowed := func(name string) bool {
		return managedAuthRelayCookieAllowed(name, branch, spec.TwoFactorEnabled, spec.AdminEnabled)
	}
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		pr.Out.Host = base.Host
		for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-CSRF-Token", "X-Real-IP", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Neon-Client-IP"} {
			pr.Out.Header.Del(h)
		}
		for _, cookie := range pr.In.Cookies() {
			if cookieAllowed(cookie.Name) {
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
			for name := range response.Header {
				if strings.HasPrefix(strings.ToLower(name), "access-control-") {
					response.Header.Del(name)
				}
			}
			cookies := response.Cookies()
			response.Header.Del("Set-Cookie")
			for _, c := range cookies {
				if cookieAllowed(c.Name) && c.Domain == "" && c.Path == "/auth/v1/"+branch && c.HttpOnly {
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
	proxy.ServeHTTP(w, r)
}
