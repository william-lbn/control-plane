package control

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestManagedAuthIntegrationSelectionKeysAndOperatorScope(t *testing.T) {
	t.Setenv("NEON_AUTH_LAB_HTTP", "true")
	t.Setenv("NEON_AUTH_INTEGRATIONS_SECRET", "auth-integrations-ci")
	p := managedAuthPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef", Generation: 1, IntegrationsSecretName: "auth-integrations-ci", Spec: ManagedAuthSpec{Database: "postgres", AllowedOrigins: []string{}, WebhookID: "delivery", WebhookEvents: []string{"send.otp", "user.created"}, PhoneNumberEnabled: true, OAuthProviders: []string{"google"}}}
	if err := validateManagedAuth(p); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []ManagedAuthSpec{{PhoneNumberEnabled: true}, {PhoneNumberExpiresIn: 60}, {WebhookID: "delivery"}, {WebhookID: "delivery", WebhookEvents: []string{"send.otp", "send.otp"}}, {WebhookID: "delivery", WebhookEvents: []string{"unknown"}}, {WebhookID: "delivery", WebhookEvents: []string{"send.otp"}, WebhookTimeoutSeconds: 11}, {OAuthProviders: []string{"google", "google"}}, {OAuthProviders: []string{"https://evil.test"}}} {
		if validateManagedAuthIntegrations(spec) == nil {
			t.Fatal("Invalid integration selection accepted")
		}
	}
	rawOld, _ := json.Marshal(ManagedAuthSpec{Database: "postgres", AllowedOrigins: []string{}})
	if string(rawOld) != `{"database":"postgres","allowed_origins":[]}` {
		t.Fatal("Legacy durable hash changed")
	}
	scope := record{"project_ids": []string{p.ProjectID}, "branch_ids": []string{p.BranchID}}
	destination := record{"url": "http://127.0.0.1:19001/hook", "addresses": []string{"127.0.0.1"}, "allowLabHTTP": true}
	oauth := record{"id": "google", "kind": "oidc", "clientID": "selected-client", "clientSecret": "selected-private-client", "authorizationURL": "http://127.0.0.1:19001/authorize", "token": destination, "jwks": destination, "issuer": "http://127.0.0.1:19001", "scopes": []string{"openid", "email", "profile"}}
	registry := record{"version": 1, "webhooks": record{"delivery": record{"project_ids": scope["project_ids"], "branch_ids": scope["branch_ids"], "destination": destination}}, "oauth": record{"google": record{"project_ids": scope["project_ids"], "branch_ids": scope["branch_ids"], "provider": oauth}, "foreign": record{"project_ids": []string{"prj_ffffffffffffffff"}, "provider": record{"clientSecret": "foreign-client-never-copied"}}}}
	uid := "immutable-registry-ci"
	immutable := true
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/neon/secrets/auth-integrations-ci" {
			w.WriteHeader(404)
			return
		}
		raw, _ := json.Marshal(registry)
		json.NewEncoder(w).Encode(record{"metadata": record{"uid": uid}, "immutable": immutable, "data": record{"config.json": base64.StdEncoding.EncodeToString(raw)}})
	}))
	defer peer.Close()
	s := &server{idempotencyKey: []byte(strings.Repeat("private-ci", 5)), kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}}
	selected, actualUID, err := s.selectedManagedAuthIntegrations(context.Background(), p)
	if err != nil || actualUID != uid {
		t.Fatal("Approved selection failed", err)
	}
	raw, _ := json.Marshal(selected)
	available := s.availableManagedAuthIntegrations(context.Background(), p)
	public, _ := json.Marshal(available)
	if strings.Contains(string(public), "private-client") || strings.Contains(string(public), "foreign") || strings.Contains(string(public), "addresses") || len(available["oauth_providers"].([]record)) != 1 {
		t.Fatal("Integration discovery leaked private/global configuration")
	}
	if strings.Contains(string(raw), "foreign-client-never-copied") {
		t.Fatal("Unselected operator credential copied")
	}
	first, _ := s.managedAuthIntegrationKey(p, "phone.otp")
	p.Generation++
	again, _ := s.managedAuthIntegrationKey(p, "phone.otp")
	hook, _ := s.managedAuthIntegrationKey(p, "webhook.ed25519")
	if first != again || first == hook || len(first) != 43 {
		t.Fatal("Integration key purpose/generation mismatch")
	}
	p.IntegrationsSecretUID = actualUID
	uid = "recreated-registry"
	if _, _, err = s.selectedManagedAuthIntegrations(context.Background(), p); err == nil {
		t.Fatal("Accepted intent silently changed operator Secret identity")
	}
	uid = actualUID
	p.BranchID = "br_ffffffffffffffff"
	other, _ := s.managedAuthIntegrationKey(p, "phone.otp")
	if other == first {
		t.Fatal("Branch keys shared")
	}
	if _, _, err = s.selectedManagedAuthIntegrations(context.Background(), p); err == nil {
		t.Fatal("Foreign branch selected credential")
	}
	p.BranchID = "br_0123456789abcdef"
	immutable = false
	if _, _, err = s.selectedManagedAuthIntegrations(context.Background(), p); err == nil {
		t.Fatal("Mutable operator registry accepted")
	}
	for _, address := range []string{"127.0.0.1", "::ffff:127.0.0.1", "169.254.169.254", "100.64.1.1", "2001:db8::1"} {
		if validManagedAuthDestination(managedAuthDestination{URL: "https://hooks.example.test/hook", Addresses: []string{address}}) {
			t.Fatal("Private/nonpublic pin admitted")
		}
	}
	for action, method := range map[string]string{"phone-number/send-otp": "POST", "phone-number/verify": "POST", ".well-known/jwks.json": "GET", "webhooks/events": "GET", "webhooks/drain": "POST", "sign-in/social": "POST", "link-social": "POST", "list-accounts": "GET", "unlink-account": "POST", "callback/google": "GET"} {
		if !managedAuthAllowedAction(action, method) || managedAuthAllowedAction(action, "DELETE") {
			t.Fatal("Integration method boundary failed", action)
		}
	}
	if managedAuthAllowedAction("get-access-token", "POST") || managedAuthAllowedAction("phone-number/request-password-reset", "POST") {
		t.Fatal("Unadvertised provider capability exposed")
	}
}

func TestManagedAuthIntegrationNativeACLGenerationAndCopiedChallenges(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	config, err := pgx.ParseConfig(dsn)
	if err != nil || config.Database != "control_ci" {
		t.Fatal("Dedicated control_ci required")
	}
	ctx := context.Background()
	owner, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	var random [8]byte
	rand.Read(random[:])
	suffix := hex.EncodeToString(random[:])
	database := "auth_events_ci_" + suffix
	if _, err = owner.Exec(ctx, "CREATE DATABASE "+pgQuote(database)); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(dsn)
	target.Path = "/" + database
	conn, err := pgx.Connect(ctx, target.String())
	if err != nil {
		t.Fatal(err)
	}
	p := managedAuthPayload{ProjectID: "prj_" + suffix, BranchID: "br_" + suffix, EndpointID: "ep_" + suffix, Generation: 1, Spec: ManagedAuthSpec{Database: database, AllowedOrigins: []string{}, PhoneNumberEnabled: true, WebhookID: "delivery", WebhookEvents: []string{"user.created", "phone_number.verified"}, OAuthProviders: []string{"google"}}}
	login := managedAuthLogin(p.BranchID)
	var runtime *pgx.Conn
	defer func() {
		if runtime != nil {
			runtime.Close(ctx)
		}
		conn.Close(ctx)
		owner.Exec(ctx, "DROP DATABASE "+pgQuote(database)+" WITH (FORCE)")
		owner.Exec(ctx, "DROP ROLE IF EXISTS "+pgQuote(login))
		owner.Exec(ctx, "DROP ROLE IF EXISTS "+pgQuote(managedAuthLogin("br_ffffffffffffffff")))
	}()
	password := randomToken(32)
	verifier, _ := scramVerifier(password)
	apply := func() {
		t.Helper()
		tx, e := conn.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = applyManagedAuthSQLTx(ctx, tx, p, verifier); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	}
	apply()
	runtimeURL, _ := url.Parse(target.String())
	runtimeURL.User = url.UserPassword(login, password)
	runtime, err = pgx.Connect(ctx, runtimeURL.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.Exec(ctx, `INSERT INTO "user"(id,name,email,"emailVerified") VALUES('owned','Owned','owned@example.test',true)`); err != nil {
		t.Fatal("Owned trigger must run under restricted LOGIN", err)
	}
	var count int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.control_webhook_event WHERE event_type='user.created' AND payload->'user'->>'id'='owned'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("Transactional producer did not enqueue")
	}
	denied := func(statement string) {
		t.Helper()
		_, e := runtime.Exec(ctx, statement)
		var native *pgconn.PgError
		if !errors.As(e, &native) || native.Code != "42501" {
			t.Fatal("Runtime escaped native ACL", e)
		}
	}
	denied(`UPDATE control_webhook_policy SET enabled=false`)
	denied(`UPDATE control_webhook_event SET payload='{}'`)
	denied(`DELETE FROM control_webhook_event`)
	denied(`SELECT control_webhook_enqueue()`)
	if _, err = runtime.Exec(ctx, `INSERT INTO control_phone_challenge VALUES('+15550000001',repeat('a',64),0,now()+interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	apply()
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.control_phone_challenge`).Scan(&count); err != nil || count != 1 {
		t.Fatal("Same-generation retry erased challenge")
	}
	if _, err = conn.Exec(ctx, `INSERT INTO neon_auth.account(id,"accountId","providerId","userId","updatedAt") VALUES('provider','subject','google','owned',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO neon_auth.account(id,"accountId","providerId","userId","updatedAt") VALUES('duplicate','subject','google','owned',now())`); err == nil {
		t.Fatal("Provider subject admitted twice")
	}
	p.Generation++
	apply()
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.control_phone_challenge`).Scan(&count); err != nil || count != 0 {
		t.Fatal("New generation kept usable SMS challenge")
	}
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.control_webhook_event WHERE state='discarded'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("Old destination still has pending delivery")
	}
	if _, err = runtime.Exec(ctx, `INSERT INTO control_phone_challenge VALUES('+15550000002',repeat('b',64),0,now()+interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	p.BranchID = "br_ffffffffffffffff"
	p.Generation = 1
	apply()
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.control_phone_challenge`).Scan(&count); err != nil || count != 0 {
		t.Fatal("Copied branch kept source's keyed challenge")
	}
	p.BranchID = "br_" + suffix
	p.Generation = 3
	p.Spec.PhoneNumberEnabled = false
	p.Spec.WebhookID = ""
	p.Spec.WebhookEvents = nil
	p.Spec.OAuthProviders = nil
	apply()
	denied(`SELECT * FROM control_phone_challenge`)
	denied(`UPDATE control_webhook_event SET state='pending'`)
	denied(`SELECT * FROM control_webhook_policy`)
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth."user" WHERE id='owned'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("Feature disable deleted application user")
	}
}
