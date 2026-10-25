package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestCatalogBackendRenamePreservesAllPrivateIdentityFiles(t *testing.T) {
	p := catalogRenamePayload{catalogPayload: bindingPayload("original_database", 1), Kind: "database", NewName: "renamed_database", ObjectOID: 1234}
	password := strings.Repeat("a", 40)
	verifier, err := scramVerifier(password)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"auth", "data_api"} {
		t.Run(kind, func(t *testing.T) {
			login := managedAuthLogin(p.BranchID)
			if kind == "data_api" {
				login = dataAPILogin(p.BranchID)
			}
			u := &url.URL{Scheme: "postgresql", Host: "proxy.neon.svc:5432", User: url.UserPassword(login, password), Path: "/" + p.Name}
			q := u.Query()
			q.Set("sslmode", "verify-full")
			q.Set("options", "endpoint="+selector(p.EndpointID))
			u.RawQuery = q.Encode()
			files := map[string]any{}
			put := func(k, value string) { files[k] = base64.StdEncoding.EncodeToString([]byte(value)) }
			put("password", password)
			put("verifier", verifier)
			put("extra-future-private-key", "never rotate this unrelated key")
			key := "config.json"
			if kind == "auth" {
				raw, _ := json.Marshal(record{"branchID": p.BranchID, "databaseURL": u.String(), "secret": "original cookie and TOTP key", "oauth": record{"clientSecret": "original OAuth secret"}})
				put(key, string(raw))
			} else {
				key = "postgrest.conf"
				put(key, "db-uri = "+strconv.Quote(u.String())+"\ndb-pool = 1\njwt-secret = \"@/run/data-api/delegation-jwks.json\"\n")
				put("delegation-seed", "original signing seed")
				put("delegation-jwks.json", `{"keys":["original public key"]}`)
			}
			original, _ := json.Marshal(files)
			data, err := catalogBackendRenameSecretData(record{"data": files}, p, catalogBackendRenameDependency{Kind: kind, Endpoint: p.EndpointID})
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(files)
			if string(after) != string(original) {
				t.Fatal("original immutable Secret mutated")
			}
			if len(data) != len(files) {
				t.Fatal("unrelated private file lost")
			}
			for k, v := range files {
				if k != key && data[k] != v {
					t.Fatalf("credential changed: %s", k)
				}
			}
			result, _ := base64.StdEncoding.DecodeString(data[key])
			if !strings.Contains(string(result), p.NewName) || strings.Contains(string(result), "/"+p.Name) {
				t.Fatal("private connection path did not migrate")
			}
			for _, change := range []func(*catalogRenamePayload){func(v *catalogRenamePayload) { v.Name = "another_database" }, func(v *catalogRenamePayload) { v.EndpointID = "ep_ffffffffffffffff" }} {
				wrong := p
				change(&wrong)
				endpoint := p.EndpointID
				if wrong.EndpointID != p.EndpointID {
					endpoint = wrong.EndpointID
				}
				if _, err = catalogBackendRenameSecretData(record{"data": files}, wrong, catalogBackendRenameDependency{Kind: kind, Endpoint: endpoint}); err == nil {
					t.Fatal("unrelated original connection adopted")
				}
			}
			put("password", strings.Repeat("b", 40))
			if _, err = catalogBackendRenameSecretData(record{"data": files}, p, catalogBackendRenameDependency{Kind: kind, Endpoint: p.EndpointID}); err == nil {
				t.Fatal("different original password accepted")
			}
		})
	}
}

func TestCatalogBackendRenameRequiresOriginalRetiredWorkload(t *testing.T) {
	p := catalogRenamePayload{catalogPayload: bindingPayload("original_database", 1), Kind: "database", NewName: "renamed_database", ObjectOID: 1234}
	const uid = "11111111-1111-4111-8111-111111111111"
	var mu sync.Mutex
	absent, pods, running, changed, unobserved := false, false, false, false, false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/pods") {
			if r.URL.Query().Get("labelSelector") != "neon-control/managed-auth="+p.BranchID {
				t.Error("wrong dormant Auth selector")
			}
			items := []any{}
			if pods {
				items = append(items, record{"metadata": record{"uid": "orphan"}})
			}
			_ = json.NewEncoder(w).Encode(record{"items": items})
			return
		}
		if absent {
			w.WriteHeader(404)
			return
		}
		actualUID := uid
		if changed {
			actualUID = "22222222-2222-4222-8222-222222222222"
		}
		replicas, observed := 0, 1
		if running {
			replicas = 1
		}
		if unobserved {
			observed = 0
		}
		_ = json.NewEncoder(w).Encode(record{"metadata": record{"uid": actualUID, "generation": 1, "labels": credentialLabels(p.ProjectID, p.EndpointID)}, "spec": record{"replicas": replicas}, "status": record{"replicas": replicas, "observedGeneration": observed}})
	}))
	defer peer.Close()
	s := &server{kube: &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}}
	for _, scenario := range []struct {
		name                                                    string
		absent, pods, running, changed, unobserved, wantFailure bool
	}{
		{name: "original_retired"}, {name: "original_missing", absent: true},
		{name: "orphan_after_deployment_deleted", absent: true, pods: true, wantFailure: true},
		{name: "original_pod_retiring", pods: true, wantFailure: true},
		{name: "externally_enabled", running: true, wantFailure: true},
		{name: "replaced_deployment", changed: true, wantFailure: true},
		{name: "unobserved_scale_down", unobserved: true, wantFailure: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			mu.Lock()
			absent, pods, running, changed, unobserved = scenario.absent, scenario.pods, scenario.running, scenario.changed, scenario.unobserved
			mu.Unlock()
			v := catalogBackendRenameDependency{Kind: "auth", Endpoint: p.EndpointID, WorkloadUID: uid}
			if absent {
				v.WorkloadUID = ""
			}
			err := s.verifyBackendRenameWorkload(context.Background(), p, &v, false)
			if (err != nil) != scenario.wantFailure {
				t.Fatal("original dormant workload proof", err)
			}
		})
	}
}

func TestCatalogBackendRenameAdmissionRetainsSplitAPIPodBoundary(t *testing.T) {
	p := catalogRenamePayload{catalogPayload: bindingPayload("original_database", 1), Kind: "database", NewName: "renamed_database", ObjectOID: 1234}
	const uid = "11111111-1111-4111-8111-111111111111"
	var mu sync.Mutex
	lists := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/pods") {
			lists++
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(record{"metadata": record{"uid": uid, "generation": 1, "labels": credentialLabels(p.ProjectID, p.EndpointID)}, "spec": record{"replicas": 0}, "status": record{"replicas": 0, "observedGeneration": 1}})
	}))
	defer peer.Close()
	s := &server{kube: &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}}
	value := catalogBackendRenameDependency{Kind: "auth", Endpoint: p.EndpointID}
	if err := s.verifyBackendRenameWorkload(context.Background(), p, &value, true); err != nil || value.WorkloadUID != uid {
		t.Fatal("API did not bind original dormant Deployment", err)
	}
	mu.Lock()
	count := lists
	mu.Unlock()
	if count != 0 {
		t.Fatal("API requested Worker-only Pod listing")
	}
	if err := s.verifyBackendRenameWorkload(context.Background(), p, &value, false); err == nil {
		t.Fatal("Worker admitted unproven Pod absence")
	}
	mu.Lock()
	count = lists
	mu.Unlock()
	if count != 1 {
		t.Fatal("Worker skipped actual Pod absence check")
	}
}

func TestCatalogBackendRenameSecretNamesFitKubernetes(t *testing.T) {
	p := catalogRenamePayload{catalogPayload: bindingPayload("original_database", 2), Kind: "database", NewName: "renamed_database", ObjectOID: 1234}
	for _, kind := range []string{"auth", "data_api"} {
		value := catalogBackendRenameDependency{Kind: kind, SecretUID: "11111111-1111-4111-8111-111111111111"}
		name := backendRenameSecretName(p, value)
		if len(name) > 63 || !functionEnvironmentNamespace.MatchString(name) || strings.Contains(name, "_") {
			t.Fatalf("invalid Kubernetes backing name for %s", kind)
		}
	}
}

func TestCatalogBackendRenamePrivateBackingRecoveryAndOriginalCreationError(t *testing.T) {
	p := catalogRenamePayload{catalogPayload: bindingPayload("original_database", 2), Kind: "database", NewName: "renamed_database", ObjectOID: 1234}
	value := catalogBackendRenameDependency{Kind: "data_api", Endpoint: p.EndpointID, SecretUID: "11111111-1111-4111-8111-111111111111"}
	labels := credentialLabels(p.ProjectID, p.EndpointID)
	labels["neon-control/branch-id"] = p.BranchID
	body := record{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true, "metadata": record{"name": backendRenameSecretName(p, value), "labels": labels}, "data": map[string]string{"private-file": base64.StdEncoding.EncodeToString([]byte("original bounded bytes"))}}
	var mu sync.Mutex
	mode := "validation"
	posts := 0
	var stored map[string]any
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			if stored == nil {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(stored)
			return
		}
		if r.Method != http.MethodPost {
			t.Error("unexpected credential replacement mutation")
			w.WriteHeader(405)
			return
		}
		posts++
		if mode == "validation" {
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(record{"message": "original creation refused"})
			return
		}
		if json.NewDecoder(r.Body).Decode(&stored) != nil {
			t.Error("invalid private backing")
		}
		stored["metadata"].(map[string]any)["uid"] = "22222222-2222-4222-8222-222222222222"
		w.WriteHeader(503) // committed original, lost reply
	}))
	defer peer.Close()
	k := &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}
	err := k.reserveCatalogBackendRenameSecret(context.Background(), p, value, body)
	var original kubeError
	if !errors.As(err, &original) || original.Status != 422 {
		t.Fatal("original creation error hidden by subsequent 404", err)
	}
	mu.Lock()
	mode = "lost"
	mu.Unlock()
	if err = k.reserveCatalogBackendRenameSecret(context.Background(), p, value, body); err != nil {
		t.Fatal("original creation reply not recovered", err)
	}
	if err = k.reserveCatalogBackendRenameSecret(context.Background(), p, value, body); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	count := posts
	stored["data"].(map[string]any)["private-file"] = base64.StdEncoding.EncodeToString([]byte("replacement"))
	mu.Unlock()
	if count != 2 {
		t.Fatal("original proposal created again")
	}
	if err = k.reserveCatalogBackendRenameSecret(context.Background(), p, value, body); err == nil {
		t.Fatal("different same-name private file adopted")
	}
	mu.Lock()
	count = posts
	mu.Unlock()
	if count != 2 {
		t.Fatal("replacement attempt mutated original credential")
	}
}

func TestCatalogBackendRenameRejectsAmbiguousPrivateConnection(t *testing.T) {
	p := catalogRenamePayload{catalogPayload: bindingPayload("original_database", 1), Kind: "database", NewName: "renamed_database", ObjectOID: 1234}
	password := strings.Repeat("a", 40)
	verifier, _ := scramVerifier(password)
	u := &url.URL{Scheme: "postgresql", Host: "proxy:5432", User: url.UserPassword(dataAPILogin(p.BranchID), password), Path: "/" + p.Name}
	q := u.Query()
	q.Set("options", "endpoint="+selector(p.EndpointID))
	q.Set("sslmode", "require")
	u.RawQuery = q.Encode()
	line := "db-uri = " + strconv.Quote(u.String())
	for _, raw := range []string{line + "\n" + line, line + "\n db-uri = " + strconv.Quote(u.String()), "db-pool = 1"} {
		data := map[string]any{}
		for k, value := range map[string]string{"password": password, "verifier": verifier, "postgrest.conf": raw} {
			data[k] = base64.StdEncoding.EncodeToString([]byte(value))
		}
		if _, err := catalogBackendRenameSecretData(record{"data": data}, p, catalogBackendRenameDependency{Kind: "data_api", Endpoint: p.EndpointID}); err == nil {
			t.Fatal("ambiguous private DSN adopted")
		}
	}
	v := catalogBackendRenameDependency{Kind: "auth", SecretUID: "11111111-1111-4111-8111-111111111111"}
	if backendRenameSecretName(p, v) != backendRenameSecretName(p, v) {
		t.Fatal("unstable original proposal")
	}
	old := backendRenameSecretName(p, v)
	p.ExpectedVersion++
	if old == backendRenameSecretName(p, v) {
		t.Fatal("another rename generation reused original private backing")
	}
}
