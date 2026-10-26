package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCatalogPlatformBootstrapUsesOwnedSecretAndOriginalScope(t *testing.T) {
	p := createPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef", EndpointType: "read_write", TenantID: strings.Repeat("a", 32), TimelineID: strings.Repeat("b", 32)}
	password := strings.Repeat("p", 40)
	probe, _ := scramVerifier(password)
	admin, _ := scramVerifier(strings.Repeat("a", 40))
	raw, err := computeConfig(p.ProjectID, p.TenantID, p.TimelineID, admin, probe, p.EndpointType)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if json.Unmarshal([]byte(raw), &document) != nil {
		t.Fatal("invalid canonical Compute config")
	}
	document["probePassword"] = "decoded document is not a Kubernetes credential"
	encode := func(v string) string { return base64.StdEncoding.EncodeToString([]byte(v)) }
	rawDocument, _ := json.Marshal(document)
	config := record{"metadata": record{"uid": "11111111-1111-4111-8111-111111111111", "resourceVersion": "3", "labels": credentialLabels(p.ProjectID, p.EndpointID)}, "data": record{"config.json": encode(string(rawDocument)), "probePassword": encode(password)}}
	credential := record{"metadata": record{"uid": "22222222-2222-4222-8222-222222222222", "labels": credentialLabels(p.ProjectID, p.EndpointID)}, "data": record{"adminVerifier": encode(admin), "probeVerifier": encode(probe), "probePassword": encode(password)}}
	var mu sync.Mutex
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != "GET" {
			t.Error("observation mutated original credential")
		}
		if strings.HasSuffix(r.URL.Path, "-credentials") {
			jsonResponse(w, 200, credential)
		} else if strings.HasSuffix(r.URL.Path, "-config") {
			jsonResponse(w, 200, config)
		} else {
			w.WriteHeader(404)
		}
	}))
	defer peer.Close()
	k := &kubeClient{base: peer.URL, namespace: "neon-ci", http: peer.Client()}
	backing, actual, err := k.readCatalogPlatformBootstrap(context.Background(), p)
	if err != nil || actual != password || backing.ConfigUID != stringVal(nested(config, "metadata", "uid")) || backing.CredentialUID != stringVal(nested(credential, "metadata", "uid")) {
		t.Fatal("original backing/private password confused with decoded document", err)
	}
	wrong := p
	wrong.TimelineID = strings.Repeat("c", 32)
	if _, _, err = k.readCatalogPlatformBootstrap(context.Background(), wrong); err == nil {
		t.Fatal("another timeline adopted")
	}
	mu.Lock()
	config["data"].(record)["probePassword"] = encode("replaced config password")
	mu.Unlock()
	if _, _, err = k.readCatalogPlatformBootstrap(context.Background(), p); err == nil {
		t.Fatal("different source config credential adopted")
	}
}
