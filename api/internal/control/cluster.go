package control

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The public compute specification is copied from the pinned laboratory Helm
// profile. It contains no credential; every identity and verifier is replaced.
//
//go:embed assets/compute-public-config.json
var computeAssets embed.FS

// The default is the audited PG16 guest for the fork release. Operators can
// replace it with another pinned image without recompiling the control plane.
const defaultComputeImage = "docker.io/williamluckyli/vm-compute-node-v16@sha256:4716bf4c0d5437b05977787398b764b5638ab42bbc657dcc6909fd39c095744d"

func vmComputeImage() string { return env("NEON_VM_COMPUTE_IMAGE", defaultComputeImage) }

const routesSecret = "neon-control-routes"

func stableSuffix(actor, scope, key string) string {
	sum := sha256.Sum256([]byte(actor + "\x00" + scope + "\x00" + key))
	return fmt.Sprintf("%x", sum[:8])
}

func stableHex(suffix, kind string) string {
	sum := sha256.Sum256([]byte("neon-control-v2:" + suffix + ":" + kind))
	return fmt.Sprintf("%x", sum[:16])
}

func kubeName(endpointID string) string { return "cp-" + strings.TrimPrefix(endpointID, "ep_") }
func selector(endpointID string) string { return "ep-" + strings.TrimPrefix(endpointID, "ep_") }

func scramVerifier(password string) (string, error) {
	if len(password) < 12 || len(password) > 256 {
		return "", errors.New("database password must be 12-256 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	salted := pbkdf2(password, salt, 4096, 32)
	clientMAC := hmac.New(sha256.New, salted)
	_, _ = clientMAC.Write([]byte("Client Key"))
	stored := sha256.Sum256(clientMAC.Sum(nil))
	serverMAC := hmac.New(sha256.New, salted)
	_, _ = serverMAC.Write([]byte("Server Key"))
	return fmt.Sprintf("SCRAM-SHA-256$4096:%s$%s:%s", base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(stored[:]), base64.StdEncoding.EncodeToString(serverMAC.Sum(nil))), nil
}

func scramMatches(password, verifier string) bool {
	parts := strings.Split(verifier, "$")
	if len(parts) != 3 || parts[0] != "SCRAM-SHA-256" {
		return false
	}
	setup := strings.Split(parts[1], ":")
	keys := strings.Split(parts[2], ":")
	if len(setup) != 2 || setup[0] != "4096" || len(keys) != 2 {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(setup[1])
	if err != nil {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(keys[0])
	if err != nil || len(expected) != sha256.Size {
		return false
	}
	clientMAC := hmac.New(sha256.New, pbkdf2(password, salt, 4096, 32))
	_, _ = clientMAC.Write([]byte("Client Key"))
	stored := sha256.Sum256(clientMAC.Sum(nil))
	return hmac.Equal(stored[:], expected)
}

func secretText(item map[string]any, key string) (string, error) {
	encoded := stringVal(nested(item, "data", key))
	if encoded == "" {
		return "", fmt.Errorf("secret key %s missing", key)
	}
	b, err := base64.StdEncoding.DecodeString(encoded)
	return string(b), err
}

func credentialLabels(projectID, endpointID string) map[string]string {
	return map[string]string{"app.kubernetes.io/part-of": "neon-control", "neon-control/project-id": projectID,
		"neon-control/endpoint-id": endpointID}
}

func owned(item map[string]any, projectID, endpointID string) bool {
	return stringVal(nested(item, "metadata", "labels", "neon-control/project-id")) == projectID &&
		stringVal(nested(item, "metadata", "labels", "neon-control/endpoint-id")) == endpointID
}

func (k *kubeClient) createOwned(ctx context.Context, kind string, body map[string]any, projectID, endpointID string) (map[string]any, error) {
	item, err := k.request(ctx, http.MethodPost, k.path(kind, ""), body)
	if err == nil {
		return item, nil
	}
	var ke kubeError
	if !errors.As(err, &ke) || ke.Status != http.StatusConflict {
		return nil, err
	}
	name := stringVal(nested(body, "metadata", "name"))
	item, err = k.request(ctx, http.MethodGet, k.path(kind, name), nil)
	if err != nil {
		return nil, err
	}
	if !owned(item, projectID, endpointID) {
		return nil, fmt.Errorf("%s/%s ownership mismatch", kind, name)
	}
	return item, nil
}

func (k *kubeClient) reserveCredentials(ctx context.Context, projectID, endpointID, password string) error {
	verifier, err := scramVerifier(password)
	if err != nil {
		return err
	}
	probePassword := randomToken(24)
	probeVerifier, err := scramVerifier(probePassword)
	if err != nil {
		return err
	}
	name := kubeName(endpointID) + "-credentials"
	data := map[string]string{"adminVerifier": base64.StdEncoding.EncodeToString([]byte(verifier)),
		"probePassword": base64.StdEncoding.EncodeToString([]byte(probePassword)),
		"probeVerifier": base64.StdEncoding.EncodeToString([]byte(probeVerifier))}
	item, err := k.createOwned(ctx, "secret", map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": name, "labels": credentialLabels(projectID, endpointID)}, "data": data}, projectID, endpointID)
	if err != nil {
		return err
	}
	stored, err := secretText(item, "adminVerifier")
	if err != nil {
		return err
	}
	if !scramMatches(password, stored) {
		return errors.New("credential reservation conflicts with a different password")
	}
	return nil
}

// Read replicas reuse the branch writer's role verifier. No plaintext database
// password is recovered or copied into control-plane metadata.
func (k *kubeClient) reserveReplicaCredentials(ctx context.Context, projectID, endpointID, writerID string) error {
	writer, err := k.request(ctx, http.MethodGet, k.path("secret", kubeName(writerID)+"-credentials"), nil)
	if err != nil || !owned(writer, projectID, writerID) {
		return errors.New("branch writer credential unavailable or not owned")
	}
	verifier, err := secretText(writer, "adminVerifier")
	if err != nil {
		return err
	}
	probePassword, err := secretText(writer, "probePassword")
	if err != nil {
		return err
	}
	writerConfig, err := k.request(ctx, http.MethodGet, k.path("secret", kubeName(writerID)+"-config"), nil)
	if err != nil || !owned(writerConfig, projectID, writerID) {
		return errors.New("branch writer compute config unavailable or not owned")
	}
	configText, err := secretText(writerConfig, "config.json")
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(configText), &config); err != nil {
		return err
	}
	probeVerifier := ""
	adminConfigVerifier := ""
	for _, entry := range nested(config, "spec", "cluster", "roles").([]any) {
		role := entry.(map[string]any)
		switch stringVal(role["name"]) {
		case "control_probe":
			probeVerifier = stringVal(role["encrypted_password"])
		case "cloud_admin":
			adminConfigVerifier = stringVal(role["encrypted_password"])
		}
	}
	if probeVerifier == "" || adminConfigVerifier != verifier {
		return errors.New("branch writer role verifier mismatch")
	}
	name := kubeName(endpointID) + "-credentials"
	item, err := k.createOwned(ctx, "secret", map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": name, "labels": credentialLabels(projectID, endpointID)},
		"data": map[string]string{"adminVerifier": base64.StdEncoding.EncodeToString([]byte(verifier)),
			"probePassword": base64.StdEncoding.EncodeToString([]byte(probePassword)),
			"probeVerifier": base64.StdEncoding.EncodeToString([]byte(probeVerifier))}}, projectID, endpointID)
	if err != nil {
		return err
	}
	stored, err := secretText(item, "adminVerifier")
	if err != nil {
		return err
	}
	if stored != verifier {
		return errors.New("replica credential reservation conflicts with branch writer")
	}
	storedProbe, err := secretText(item, "probePassword")
	if err != nil {
		return err
	}
	if storedProbe != probePassword {
		return errors.New("replica probe credential conflicts with branch writer")
	}
	storedVerifier, err := secretText(item, "probeVerifier")
	if err != nil || storedVerifier != probeVerifier {
		return errors.New("replica probe verifier conflicts with branch writer")
	}
	return nil
}

func (k *kubeClient) createTenant(ctx context.Context, tenantID string) error {
	_, err := k.serviceRequest(ctx, "storage-controller", 1234, "v1/tenant", http.MethodPost, map[string]any{"new_tenant_id": tenantID})
	var ke kubeError
	if errors.As(err, &ke) && ke.Status == http.StatusConflict {
		_, err = k.serviceRequest(ctx, "storage-controller", 1234, "control/v1/tenant/"+tenantID, http.MethodGet, nil)
	}
	return err
}

func (k *kubeClient) createTimeline(ctx context.Context, tenantID, timelineID, parentID, parentLSN string) error {
	body := map[string]any{"new_timeline_id": timelineID, "pg_version": 16}
	if parentID != "" {
		body["ancestor_timeline_id"] = parentID
		body["ancestor_start_lsn"] = parentLSN
	}
	_, err := k.serviceRequest(ctx, "storage-controller", 1234, "v1/tenant/"+tenantID+"/timeline", http.MethodPost, body)
	var ke kubeError
	if errors.As(err, &ke) && ke.Status == http.StatusConflict {
		item, getErr := k.serviceRequest(ctx, "storage-controller", 1234,
			"control/v1/tenant/"+tenantID+"/timeline/"+timelineID, http.MethodGet, nil)
		if getErr != nil {
			return getErr
		}
		if parentID != "" && stringVal(item["ancestor_timeline_id"]) != "" && stringVal(item["ancestor_timeline_id"]) != parentID {
			return errors.New("timeline parent ownership mismatch")
		}
		return nil
	}
	return err
}

func (k *kubeClient) parentLSN(ctx context.Context, tenantID, timelineID string) (string, error) {
	item, err := k.serviceRequest(ctx, "pageserver-managed", 9898,
		"v1/tenant/"+tenantID+"/timeline/"+timelineID, http.MethodGet, nil)
	if err != nil {
		return "", err
	}
	lsn := stringVal(item["last_record_lsn"])
	if lsn == "" {
		return "", errors.New("parent timeline has no last_record_lsn")
	}
	return lsn, nil
}

func computeConfig(projectID, tenantID, timelineID, adminVerifier, probeVerifier, endpointType string) (string, error) {
	b, err := computeAssets.ReadFile("assets/compute-public-config.json")
	if err != nil {
		return "", err
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return "", err
	}
	spec := nested(data, "spec").(map[string]any)
	if endpointType == "read_only" {
		spec["mode"] = "Replica"
	} else if endpointType != "" && endpointType != "read_write" {
		return "", errors.New("unsupported endpoint type")
	}
	spec["operation_uuid"] = fmt.Sprintf("%s-%s-%s-%s-%s", randomToken(4), randomToken(2), randomToken(2), randomToken(2), randomToken(6))
	spec["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	cluster := nested(spec, "cluster").(map[string]any)
	cluster["cluster_id"], cluster["name"] = projectID, projectID
	roles := cluster["roles"].([]any)
	for _, role := range roles {
		item := role.(map[string]any)
		if item["name"] == "cloud_admin" {
			item["encrypted_password"] = adminVerifier
		}
	}
	cluster["roles"] = append(roles, map[string]any{"name": "control_probe", "encrypted_password": probeVerifier, "options": nil})
	for _, setting := range cluster["settings"].([]any) {
		item := setting.(map[string]any)
		switch item["name"] {
		case "neon.tenant_id":
			item["value"] = tenantID
		case "neon.timeline_id":
			item["value"] = timelineID
		case "neon.pageserver_connstring":
			item["value"] = "host=pageserver-managed port=6400"
		case "shared_preload_libraries":
			if !strings.Contains(stringVal(item["value"]), "ulid") {
				item["value"] = stringVal(item["value"]) + ",ulid"
			}
		}
	}
	if endpointType == "read_only" {
		// Neon local control-plane uses PostgreSQL physical replication from
		// Safekeepers. Replica mode alone only creates standby.signal; without
		// primary_conninfo it accepts stale reads indefinitely.
		settings := cluster["settings"].([]any)
		settings = append(settings,
			map[string]any{"name": "primary_conninfo", "vartype": "string",
				"value": fmt.Sprintf("host=safekeeper1,safekeeper2,safekeeper3 port=5454,5454,5454 options='-c timeline_id=%s tenant_id=%s' application_name=replica replication=true", timelineID, tenantID)},
			map[string]any{"name": "primary_slot_name", "vartype": "string", "value": "repl_" + timelineID + "_"},
			map[string]any{"name": "recovery_prefetch", "vartype": "enum", "value": "off"})
		cluster["settings"] = settings
	}
	encoded, err := json.Marshal(data)
	return string(encoded), err
}

type createPayload struct {
	ProjectID        string `json:"project_id"`
	BranchID         string `json:"branch_id"`
	EndpointID       string `json:"endpoint_id"`
	EndpointType     string `json:"endpoint_type,omitempty"`
	TenantID         string `json:"tenant_id"`
	TimelineID       string `json:"timeline_id"`
	ParentTimelineID string `json:"parent_timeline_id"`
	ParentLSN        string `json:"parent_lsn"`
	CatalogSpec      string `json:"-"` // Runtime projection from branch intent; never persisted in Operation.
	MinCPU           int    `json:"min_cpu_milli"`
	MaxCPU           int    `json:"max_cpu_milli"`
	MinMem           int    `json:"min_memory_mib"`
	MaxMem           int    `json:"max_memory_mib"`
}

// vmWaitError contains only a Kubernetes phase, never a Secret or SQL value.
type vmWaitError struct {
	phase string
	cause error
}

func (e vmWaitError) Error() string {
	return "NeonVM did not become Running (last phase: " + e.phase + ")"
}
func (e vmWaitError) Unwrap() error { return e.cause }

func (k *kubeClient) createCompute(ctx context.Context, p createPayload) (map[string]any, error) {
	name := kubeName(p.EndpointID)
	credential, err := k.request(ctx, http.MethodGet, k.path("secret", name+"-credentials"), nil)
	if err != nil || !owned(credential, p.ProjectID, p.EndpointID) {
		return nil, errors.New("endpoint credential reference unavailable or not owned")
	}
	admin, err := secretText(credential, "adminVerifier")
	if err != nil {
		return nil, err
	}
	probePassword, err := secretText(credential, "probePassword")
	if err != nil {
		return nil, err
	}
	probeVerifier, err := secretText(credential, "probeVerifier")
	if err != nil {
		// Credentials reserved before migration 005 do not include the
		// verifier. Reuse the already-persisted role if a config exists;
		// otherwise a first-time writer may generate it once.
		if p.EndpointType != "read_write" {
			return nil, err
		}
		existing, getErr := k.request(ctx, http.MethodGet, k.path("secret", name+"-config"), nil)
		if getErr == nil {
			if !owned(existing, p.ProjectID, p.EndpointID) {
				return nil, errors.New("existing compute config not owned")
			}
			stored, readErr := secretText(existing, "config.json")
			if readErr != nil {
				return nil, readErr
			}
			var decoded map[string]any
			if readErr := json.Unmarshal([]byte(stored), &decoded); readErr != nil {
				return nil, readErr
			}
			for _, entry := range nested(decoded, "spec", "cluster", "roles").([]any) {
				role := entry.(map[string]any)
				if role["name"] == "control_probe" {
					probeVerifier = stringVal(role["encrypted_password"])
				}
			}
			if probeVerifier == "" {
				return nil, errors.New("existing compute probe role missing")
			}
		} else {
			var ke kubeError
			if !errors.As(getErr, &ke) || ke.Status != http.StatusNotFound {
				return nil, getErr
			}
			probeVerifier, err = scramVerifier(probePassword)
			if err != nil {
				return nil, err
			}
		}
	}
	config, err := computeConfig(p.ProjectID, p.TenantID, p.TimelineID, admin, probeVerifier, p.EndpointType)
	if err != nil {
		return nil, err
	}
	controlKey, err := k.computeControlKey(ctx, p.ProjectID, p.EndpointID)
	if err != nil {
		return nil, err
	}
	var computeConfigData map[string]any
	if err = json.Unmarshal([]byte(config), &computeConfigData); err != nil {
		return nil, err
	}
	installComputeJWK(computeConfigData, controlKey)
	tlsIdentity, err := k.computeTLSIdentity(ctx, p.ProjectID, p.EndpointID, env("NEON_COMPUTE_CONTROL_HOST", env("NEON_PROXY_HOST", "192.168.146.100")))
	if err != nil {
		return nil, err
	}
	installComputeTLS(computeConfigData)
	encodedConfig, err := json.Marshal(computeConfigData)
	if err != nil {
		return nil, err
	}
	config = string(encodedConfig)
	if p.CatalogSpec != "" {
		var data map[string]any
		var catalog map[string]any
		if err = json.Unmarshal([]byte(config), &data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(p.CatalogSpec), &catalog); err != nil {
			return nil, err
		}
		cluster := nested(data, "spec", "cluster").(map[string]any)
		roles := cluster["roles"].([]any)
		extra, _ := catalog["roles"].([]any)
		cluster["roles"] = append(roles, extra...)
		cluster["databases"] = catalog["databases"]
		data["spec"].(map[string]any)["delta_operations"] = catalog["delta_operations"]
		encoded, e := json.Marshal(data)
		if e != nil {
			return nil, e
		}
		config = string(encoded)
	}
	labels := credentialLabels(p.ProjectID, p.EndpointID)
	configSecret, err := k.createOwned(ctx, "secret", map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": name + "-config", "labels": labels},
		"data": map[string]string{"config.json": base64.StdEncoding.EncodeToString([]byte(config)),
			"probePassword": base64.StdEncoding.EncodeToString([]byte(probePassword)), "control.crt": tlsIdentity["controlCert"], "control.key": tlsIdentity["controlTLSKey"]}}, p.ProjectID, p.EndpointID)
	if err != nil {
		return nil, err
	}
	storedConfig, err := secretText(configSecret, "config.json")
	if err != nil {
		return nil, err
	}
	var persisted map[string]any
	if err := json.Unmarshal([]byte(storedConfig), &persisted); err != nil {
		return nil, err
	}
	if stringVal(nested(persisted, "spec", "cluster", "cluster_id")) != p.ProjectID {
		return nil, errors.New("compute config project mismatch")
	}
	if p.EndpointType == "read_only" && nested(persisted, "spec", "mode") != "Replica" {
		return nil, errors.New("compute config replica mode mismatch")
	}
	roles := map[string]string{}
	for _, entry := range nested(persisted, "spec", "cluster", "roles").([]any) {
		role := entry.(map[string]any)
		roles[stringVal(role["name"])] = stringVal(role["encrypted_password"])
	}
	if roles["cloud_admin"] == "" || roles["control_probe"] == "" {
		return nil, errors.New("compute config roles missing")
	}
	if roles["cloud_admin"] != admin || roles["control_probe"] != probeVerifier {
		return nil, errors.New("compute config role verifier mismatch")
	}
	service := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": name, "labels": labels},
		"spec": map[string]any{"selector": map[string]string{"vm.neon.tech/name": name},
			"ports": []any{map[string]any{"name": "postgres", "port": 55433, "targetPort": "postgres"}}}}
	if _, err := k.createOwned(ctx, "service", service, p.ProjectID, p.EndpointID); err != nil {
		return nil, err
	}
	vmLabels := credentialLabels(p.ProjectID, p.EndpointID)
	vmLabels["autoscaling.neon.tech/enabled"] = "true"
	bounds, _ := json.Marshal(map[string]any{"min": map[string]string{"cpu": fmt.Sprint(p.MinCPU / 1000), "mem": fmt.Sprintf("%dGi", p.MinMem/1024)},
		"max": map[string]string{"cpu": fmt.Sprint(p.MaxCPU / 1000), "mem": fmt.Sprintf("%dGi", p.MaxMem/1024)}})
	vm := map[string]any{"apiVersion": "vm.neon.tech/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": name, "labels": vmLabels,
			"annotations": map[string]string{"autoscaling.neon.tech/bounds": string(bounds)}},
		"spec": map[string]any{"schedulerName": "autoscale-scheduler", "enableSSH": true,
			"guest": map[string]any{"cpus": map[string]int{"min": p.MinCPU / 1000, "use": p.MinCPU / 1000, "max": p.MaxCPU / 1000},
				"memorySlotSize": "1Gi", "memorySlots": map[string]int{"min": p.MinMem / 1024, "use": p.MinMem / 1024, "max": p.MaxMem / 1024},
				"rootDisk": map[string]string{"image": vmComputeImage(), "size": "8Gi"},
				"args": []string{"--pgdata", "/var/db/postgres/compute", "-C", "postgresql://cloud_admin@localhost:55433/postgres",
					"-b", "/usr/local/bin/postgres", "--compute-id", name, "--config", "/run/neon-lab/config.json",
					"--filecache-connstr", "host=localhost port=55433 dbname=postgres user=cloud_admin sslmode=disable application_name=vm-monitor", "--dev"},
				"env": []any{map[string]string{"name": "PG_VERSION", "value": "16"}, map[string]string{"name": "TENANT_ID", "value": p.TenantID},
					map[string]string{"name": "TIMELINE_ID", "value": p.TimelineID}, map[string]string{"name": "ALLOW_LAB_AUTOBOOTSTRAP", "value": "NO"},
					map[string]string{"name": "AUTOSCALING", "value": "1"}},
				"ports": []any{map[string]any{"name": "postgres", "port": 55433}, map[string]any{"name": "metrics", "port": 9100},
					map[string]any{"name": "monitor", "port": 10301}, map[string]any{"name": "compute-control", "port": 3080}}},
			"extraNetwork": map[string]bool{"enable": true},
			"disks": []any{map[string]any{"name": "compute-config", "mountPath": "/run/neon-lab",
				"secret": map[string]any{"secretName": name + "-config", "items": []any{map[string]string{"key": "config.json", "path": "config.json"}, map[string]string{"key": "control.crt", "path": "control.crt"}, map[string]string{"key": "control.key", "path": "control.key"}}}}}}}
	created, err := k.createOwned(ctx, "vm", vm, p.ProjectID, p.EndpointID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"vm": created, "roles": roles, "probe_password": probePassword}, nil
}

func (k *kubeClient) waitCompute(ctx context.Context, projectID, endpointID string) (map[string]any, error) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	lastPhase := "unknown"
	for {
		vm, err := k.request(ctx, http.MethodGet, k.path("vm", kubeName(endpointID)), nil)
		if err == nil {
			if !owned(vm, projectID, endpointID) {
				return nil, errors.New("VM ownership mismatch")
			}
			lastPhase = stringVal(nested(vm, "status", "phase"))
			if lastPhase == "Running" {
				return vm, nil
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, vmWaitError{phase: lastPhase, cause: ctx.Err()}
			}
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, vmWaitError{phase: lastPhase, cause: ctx.Err()}
		case <-ticker.C:
		}
	}
}

func vmTemplate(vm map[string]any) map[string]any {
	meta := vm["metadata"].(map[string]any)
	labels := map[string]any{}
	for _, key := range []string{"app.kubernetes.io/part-of", "neon-control/project-id", "neon-control/endpoint-id", "autoscaling.neon.tech/enabled"} {
		labels[key] = nested(meta, "labels", key)
	}
	spec := vm["spec"].(map[string]any)
	delete(spec, "targetRevision")
	guest := spec["guest"].(map[string]any)
	for _, key := range []string{"cpus", "memorySlots"} {
		resource := guest[key].(map[string]any)
		resource["use"] = resource["min"]
	}
	return map[string]any{"apiVersion": "vm.neon.tech/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": meta["name"], "labels": labels,
			"annotations": map[string]any{"autoscaling.neon.tech/bounds": nested(meta, "annotations", "autoscaling.neon.tech/bounds")}}, "spec": spec}
}

func (k *kubeClient) publishRoute(ctx context.Context, p createPayload, vm map[string]any, roles map[string]string) error {
	if !owned(vm, p.ProjectID, p.EndpointID) {
		return errors.New("route VM ownership mismatch")
	}
	name := kubeName(p.EndpointID)
	route := map[string]any{"project_id": p.ProjectID, "branch_id": p.BranchID, "tenant_id": p.TenantID,
		"role": "cloud_admin", "verifier": roles["cloud_admin"], "roles": roles, "kind": "neonvm", "workload": name,
		"address": name + "." + k.namespace + ".svc.cluster.local:55433", "pageserver_node_id": 2,
		"allowed_ips": []string{"0.0.0.0/0"}, "vm_template": vmTemplate(vm)}
	for attempt := 0; attempt < 6; attempt++ {
		secret, err := k.request(ctx, http.MethodGet, k.path("secret", routesSecret), nil)
		if err != nil {
			return err
		}
		raw, err := secretText(secret, "routes.json")
		if err != nil {
			return err
		}
		routes := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &routes); err != nil {
			return err
		}
		key := selector(p.EndpointID)
		if previous, ok := routes[key].(map[string]any); ok &&
			(stringVal(previous["project_id"]) != p.ProjectID || stringVal(previous["branch_id"]) != p.BranchID) {
			return errors.New("endpoint selector ownership mismatch")
		}
		// Service identities are SQL-created NOSUPERUSER/NOBYPASSRLS roles,
		// deliberately absent from Compute's privileged ordinary role spec.
		// Preserve only this branch's reserved identity when republishing a
		// cold-start template; every other role still follows the catalog spec.
		if previous, ok := routes[key].(map[string]any); ok {
			if previousRoles, ok := previous["roles"].(map[string]any); ok {
				if verifier := stringVal(previousRoles[dataAPILogin(p.BranchID)]); verifier != "" {
					roles[dataAPILogin(p.BranchID)] = verifier
				}
			}
		}
		routes[key] = route
		encoded, _ := json.Marshal(routes)
		secret["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(encoded)
		_, err = k.request(ctx, http.MethodPut, k.path("secret", routesSecret), secret)
		var ke kubeError
		if errors.As(err, &ke) && ke.Status == http.StatusConflict {
			continue
		}
		return err
	}
	return errors.New("route registry update conflict")
}
