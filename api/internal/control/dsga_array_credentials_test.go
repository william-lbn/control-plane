package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neondatabase/autoscaling/pkg/dsga/owner"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func arrayBrokerEnvelope(claim owner.Ownership, path string) dsgaArrayCredentialEnvelope {
	return dsgaArrayCredentialEnvelope{ProtocolVersion: 1, Owner: claim, Request: dsgaArrayRequestBinding{1, http.MethodPost, path, strings.Repeat("a", 64)}}
}
func arrayBrokerURL(address string) string {
	return strings.TrimSuffix(address, "/dsga-credentials") + "/dsga-array-credentials"
}
func arrayBrokerToken(t *testing.T, data []byte, key ed25519.PrivateKey) (map[string]json.RawMessage, []string) {
	t.Helper()
	var reply struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(data, &reply) != nil {
		t.Fatal("invalid broker response")
	}
	parts := strings.Split(reply.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatal("missing JWT")
	}
	sig, e := base64.RawURLEncoding.DecodeString(parts[2])
	if e != nil || !ed25519.Verify(key.Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("broker signature invalid")
	}
	payload, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		t.Fatal(e)
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(payload, &claims) != nil {
		t.Fatal("bad signed payload")
	}
	return claims, parts
}
func TestDSGAArrayBrokerBoundScopeRequestAndLegacyCompatibility(t *testing.T) {
	for _, path := range []string{"/dsg/v1/resource/array-authorize", "/dsg/v1/resource/array-observe", "/dsg/v1/resource/array-replay-original"} {
		t.Run(strings.TrimPrefix(path, "/dsg/v1/resource/"), func(t *testing.T) {
			f, _, claim, key, client, address := newBrokerTest(t)
			envelope := arrayBrokerEnvelope(claim, path)
			raw, _ := json.Marshal(envelope)
			code, data := brokerRequest(t, client, arrayBrokerURL(address), raw)
			if code != http.StatusOK {
				t.Fatalf("array broker status %d", code)
			}
			var reply struct {
				Scope    string                  `json:"scope"`
				Request  dsgaArrayRequestBinding `json:"request"`
				Epoch    string                  `json:"issuer_epoch"`
				Compute  string                  `json:"compute_id"`
				VM       string                  `json:"vm_uid"`
				Instance string                  `json:"compute_instance_id"`
			}
			if json.Unmarshal(data, &reply) != nil || reply.Scope != dsgaArrayAuthorityScope || reply.Request != envelope.Request || reply.Epoch != "1" || reply.Compute != brokerTestWorkload || reply.VM != "vm-uid" || reply.Instance != "compute-1" {
				t.Fatal("new grant did not echo exact request/owner binding")
			}
			claims, _ := arrayBrokerToken(t, data, key)
			var signed dsgaArrayRequestBinding
			var scope, aud, epoch string
			var exp, nbf int64
			if json.Unmarshal(claims["request"], &signed) != nil || json.Unmarshal(claims["scope"], &scope) != nil || json.Unmarshal(claims["aud"], &aud) != nil || json.Unmarshal(claims["issuer_epoch"], &epoch) != nil || json.Unmarshal(claims["exp"], &exp) != nil || json.Unmarshal(claims["nbf"], &nbf) != nil || signed != envelope.Request || scope != dsgaArrayAuthorityScope || aud != "neon-dsga" || epoch != "1" || exp-nbf != 62 || exp < time.Now().Unix() {
				t.Fatal("signed request/scope/lifetime differs")
			}
			oldRaw, _ := json.Marshal(claim)
			oldCode, oldData := brokerRequest(t, client, address, oldRaw)
			if oldCode != 200 {
				t.Fatal("legacy issuer regressed")
			}
			var oldReply map[string]json.RawMessage
			_ = json.Unmarshal(oldData, &oldReply)
			if len(oldReply) != 7 || oldReply["scope"] != nil || oldReply["request"] != nil {
				t.Fatal("legacy grant JSON shape changed")
			}
			oldClaims, _ := arrayBrokerToken(t, oldData, key)
			if oldClaims["request"] != nil || string(oldClaims["scope"]) != `"compute_ctl:dsga"` {
				t.Fatal("generic token acquired array binding")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.secretWrites != 0 || !f.validAPIAuth {
				t.Fatal("issuer wrote/rotated private key or confused Kube identity")
			}
		})
	}
}
func TestDSGAArrayBrokerRejectsProtocolScopeAndRequestAliases(t *testing.T) {
	_, _, claim, _, client, address := newBrokerTest(t)
	base := arrayBrokerEnvelope(claim, "/dsg/v1/resource/array-authorize")
	cases := map[string]func(*dsgaArrayCredentialEnvelope){
		"protocol": func(e *dsgaArrayCredentialEnvelope) { e.ProtocolVersion = 2 }, "binding-protocol": func(e *dsgaArrayCredentialEnvelope) { e.Request.ProtocolVersion = 0 },
		"GET": func(e *dsgaArrayCredentialEnvelope) { e.Request.Method = http.MethodGet }, "method-case": func(e *dsgaArrayCredentialEnvelope) { e.Request.Method = "post" },
		"apply": func(e *dsgaArrayCredentialEnvelope) { e.Request.Path = "/dsg/v1/resource/apply" }, "path-case": func(e *dsgaArrayCredentialEnvelope) { e.Request.Path = "/dsg/v1/resource/Array-authorize" },
		"path-slash": func(e *dsgaArrayCredentialEnvelope) { e.Request.Path += "/" }, "path-query": func(e *dsgaArrayCredentialEnvelope) { e.Request.Path += "?x=1" }, "path-escape": func(e *dsgaArrayCredentialEnvelope) { e.Request.Path = "/dsg/v1/resource/%61rray-authorize" },
		"digest-empty": func(e *dsgaArrayCredentialEnvelope) { e.Request.BodySHA256 = "" }, "digest-zero": func(e *dsgaArrayCredentialEnvelope) { e.Request.BodySHA256 = strings.Repeat("0", 64) }, "digest-upper": func(e *dsgaArrayCredentialEnvelope) { e.Request.BodySHA256 = strings.Repeat("A", 64) }, "digest-long": func(e *dsgaArrayCredentialEnvelope) { e.Request.BodySHA256 = strings.Repeat("a", 65) }, "digest-invalid": func(e *dsgaArrayCredentialEnvelope) { e.Request.BodySHA256 = strings.Repeat("g", 64) },
	}
	for n, mutate := range cases {
		t.Run(n, func(t *testing.T) {
			e := base
			mutate(&e)
			raw, _ := json.Marshal(e)
			code, data := brokerRequest(t, client, arrayBrokerURL(address), raw)
			if code != http.StatusBadRequest || bytes.Contains(data, []byte("access_token")) {
				t.Fatal("invalid request was signed")
			}
		})
	}
	raw, _ := json.Marshal(base)
	for n, bad := range map[string][]byte{
		"unknown-scope":      bytes.Replace(raw, []byte(`"protocol_version":1`), []byte(`"scope":"compute_ctl:dsga","protocol_version":1`), 1),
		"duplicate-envelope": bytes.Replace(raw, []byte(`"protocol_version":1`), []byte(`"protocol_version":1,"protocol_version":1`), 1),
		"duplicate-binding":  bytes.Replace(raw, []byte(`"method":"POST"`), []byte(`"method":"POST","method":"POST"`), 1),
		"trailing":           append(append([]byte(nil), raw...), []byte("{}")...), "oversize": bytes.Repeat([]byte("x"), 8193),
	} {
		t.Run(n, func(t *testing.T) {
			code, data := brokerRequest(t, client, arrayBrokerURL(address), bad)
			if code != 400 || bytes.Contains(data, []byte("access_token")) {
				t.Fatal("malformed scope/binding was signed")
			}
		})
	}

	// 三层精确wire键：Go大小写alias、语义双拼写、missing/null都不得静默覆盖。
	for n, bad := range map[string][]byte{
		"top-wrong-case":         bytes.Replace(raw, []byte(`"protocol_version"`), []byte(`"Protocol_Version"`), 1),
		"top-alias-dual":         bytes.Replace(raw, []byte(`"protocol_version":1`), []byte(`"protocol_version":1,"Protocol_Version":1`), 1),
		"owner-wrong-case":       bytes.Replace(raw, []byte(`"namespace"`), []byte(`"Namespace"`), 1),
		"owner-alias-dual":       bytes.Replace(raw, []byte(`"namespace":"neon"`), []byte(`"namespace":"neon","Namespace":"neon"`), 1),
		"binding-wrong-case":     bytes.Replace(raw, []byte(`"method"`), []byte(`"Method"`), 1),
		"binding-alias-dual":     bytes.Replace(raw, []byte(`"method":"POST"`), []byte(`"method":"POST","Method":"GET"`), 1),
		"missing-binding-method": bytes.Replace(raw, []byte(`"method":"POST",`), nil, 1),
		"null-binding-method":    bytes.Replace(raw, []byte(`"method":"POST"`), []byte(`"method":null`), 1),
		"null-binding-path":      bytes.Replace(raw, []byte(`"path":"/dsg/v1/resource/array-authorize"`), []byte(`"path":null`), 1),
		"null-owner-epoch":       bytes.Replace(raw, []byte(`"issuer_epoch":"1"`), []byte(`"issuer_epoch":null`), 1),
		"null-owner":             []byte(`{"protocol_version":1,"owner":null,"request":{"protocol_version":1,"method":"POST","path":"/dsg/v1/resource/array-authorize","body_sha256":"` + strings.Repeat("a", 64) + `"}}`),
		"null-request":           []byte(`{"protocol_version":1,"owner":{},"request":null}`),
		"missing-request":        []byte(`{"protocol_version":1,"owner":{}}`),
		"null-protocol":          bytes.Replace(raw, []byte(`"protocol_version":1`), []byte(`"protocol_version":null`), 1),
	} {
		t.Run(n, func(t *testing.T) {
			code, data := brokerRequest(t, client, arrayBrokerURL(address), bad)
			if code != http.StatusBadRequest || bytes.Contains(data, []byte("access_token")) {
				t.Fatal("alias/missing/null wire was signed")
			}
		})
	}
	t.Run("bare-empty-query-with-current-projected-owner", func(t *testing.T) {
		code, data := brokerRequest(t, client, arrayBrokerURL(address)+"?", raw)
		if code != http.StatusForbidden || bytes.Contains(data, []byte("access_token")) {
			t.Fatal("bare empty query was accepted")
		}
	})
	t.Run("generic-owner-body-not-array-envelope", func(t *testing.T) {
		b, _ := json.Marshal(claim)
		code, _ := brokerRequest(t, client, arrayBrokerURL(address), b)
		if code != 400 {
			t.Fatal("generic body upgraded to array token")
		}
	})
	t.Run("array-envelope-not-generic-body", func(t *testing.T) {
		code, _ := brokerRequest(t, client, address, raw)
		if code != 400 {
			t.Fatal("array envelope entered generic issuer")
		}
	})
}
func TestDSGAArrayBrokerRequiresCurrentOriginalOwnerAndReadAfterKeyFence(t *testing.T) {
	for _, scenario := range []string{"VMUID", "LeaseUID", "epoch", "holder", "namespace", "workload", "expired-lease", "bootstrap-held", "after-key-owner-change"} {
		t.Run(scenario, func(t *testing.T) {
			f, store, claim, _, client, address := newBrokerTest(t)
			envelope := arrayBrokerEnvelope(claim, "/dsg/v1/resource/array-observe")
			switch scenario {
			case "VMUID":
				envelope.Owner.VMUID = "other-vm"
			case "LeaseUID":
				envelope.Owner.LeaseUID = "other-lease"
			case "epoch":
				envelope.Owner.IssuerEpoch++
			case "holder":
				envelope.Owner.HolderID = "dsga-" + strings.Repeat("b", 32) + ".other-pod"
			case "namespace":
				envelope.Owner.Namespace = "foreign"
			case "workload":
				envelope.Owner.VMName = "cp-fedcba9876543210"
			case "expired-lease":
				f.mu.Lock()
				old := metav1.NewMicroTime(time.Now().Add(-time.Hour))
				f.lease.Spec.RenewTime = &old
				f.mu.Unlock()
			case "bootstrap-held":
				f.mu.Lock()
				f.vm.Spec.DSGABootstrap.Ready = false
				f.mu.Unlock()
			case "after-key-owner-change":
				f.mu.Lock()
				f.afterKeyRead = func() {
					snapshot, e := store.Load(context.Background())
					if e != nil {
						t.Error(e)
						return
					}
					if _, e = store.Takeover(context.Background(), snapshot, snapshot.State().Owner, "other-owner"); e != nil {
						t.Error(e)
					}
				}
				f.mu.Unlock()
			}
			raw, _ := json.Marshal(envelope)
			code, data := brokerRequest(t, client, arrayBrokerURL(address), raw)
			if code == 200 || bytes.Contains(data, []byte("access_token")) {
				t.Fatal("foreign/stale owner signed")
			}
		})
	}
}
func TestDSGAArrayBrokerSignedRequestCannotBeTampered(t *testing.T) {
	_, _, claim, key, client, address := newBrokerTest(t)
	raw, _ := json.Marshal(arrayBrokerEnvelope(claim, "/dsg/v1/resource/array-authorize"))
	code, data := brokerRequest(t, client, arrayBrokerURL(address), raw)
	if code != 200 {
		t.Fatal(code)
	}
	claims, parts := arrayBrokerToken(t, data, key)
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	for _, field := range []string{"protocol_version", "method", "path", "body_sha256", "scope", "issuer_epoch"} {
		t.Run(field, func(t *testing.T) {
			copied := map[string]json.RawMessage{}
			for k, v := range claims {
				copied[k] = append([]byte(nil), v...)
			}
			if field == "scope" {
				copied[field] = json.RawMessage(`"compute_ctl:dsga"`)
			} else if field == "issuer_epoch" {
				copied[field] = json.RawMessage(`"2"`)
			} else {
				var binding dsgaArrayRequestBinding
				_ = json.Unmarshal(copied["request"], &binding)
				switch field {
				case "protocol_version":
					binding.ProtocolVersion = 2
				case "method":
					binding.Method = "GET"
				case "path":
					binding.Path = "/dsg/v1/resource/apply"
				case "body_sha256":
					binding.BodySHA256 = strings.Repeat("b", 64)
				}
				copied["request"], _ = json.Marshal(binding)
			}
			payload, _ := json.Marshal(copied)
			changed := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload)
			if ed25519.Verify(key.Public().(ed25519.PublicKey), []byte(changed), signature) {
				t.Fatal("signed binding tamper accepted")
			}
		})
	}
}
func TestDSGAArrayBrokerDefaultClosedAndGenericBearerNotAuthority(t *testing.T) {
	f, _, claim, _, client, address := newBrokerTest(t)
	mux, e := (&computeGateway{kube: f.kube}).routes("", "")
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/computes/"+brokerTestWorkload+"/dsga-array-credentials", nil))
	if w.Code != http.StatusNotFound {
		t.Fatal("default exposed array issuer")
	}
	raw, _ := json.Marshal(arrayBrokerEnvelope(claim, "/dsg/v1/resource/array-authorize"))
	for _, suffix := range []string{"", "?scope=admin"} {
		request, e := http.NewRequest(http.MethodPost, arrayBrokerURL(address)+suffix, bytes.NewReader(raw))
		if e != nil {
			t.Fatal(e)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer generic-tenant-or-service-account-token")
		response, e := client.Do(request)
		if e != nil {
			t.Fatal(e)
		}
		body, e := io.ReadAll(response.Body)
		response.Body.Close()
		if e != nil {
			t.Fatal(e)
		}
		if response.StatusCode != http.StatusForbidden || bytes.Contains(body, []byte("access_token")) {
			t.Fatal("generic bearer/query acquired array authority")
		}
	}
}
