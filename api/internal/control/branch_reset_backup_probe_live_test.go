//go:build live

package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type resetBackupLiveFixture struct {
	Purpose           string   `json:"purpose"`
	Project           string   `json:"project"`
	SourceBranch      string   `json:"source_branch"`
	SourceEndpoint    string   `json:"source_endpoint"`
	RetainedBranch    string   `json:"retained_branch"`
	OriginalOperation string   `json:"original_operation"`
	Database          string   `json:"database"`
	CIDRs             []string `json:"cidrs"`
	Output            string   `json:"output"`
}

// Opt-in trusted owned-fixture acceptance only. This reuses an already-retained
// data-only historical fork; it never creates a branch, rewinds live metadata,
// migrates the control database, or starts the original public Endpoint.
func TestBranchResetBackupReadonlyNativeLive(t *testing.T) {
	var f resetBackupLiveFixture
	raw, err := os.ReadFile(os.Getenv("NEON_BRANCH_RESET_BACKUP_LIVE_FIXTURE"))
	if err != nil || json.Unmarshal(raw, &f) != nil || f.Purpose != "owned-static-backup-inspection-v1" || !dataBranchID.MatchString(f.SourceBranch) || !dataBranchID.MatchString(f.RetainedBranch) || f.SourceBranch == f.RetainedBranch || !functionWriterEndpointID.MatchString(f.SourceEndpoint) || !branchResetOperationID.MatchString(f.OriginalOperation) || !validPGIdentifier(f.Database) || protectedPGDatabase(f.Database) || !filepath.IsAbs(f.Output) {
		t.Fatal("explicit bounded owned Linux fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	db, err := pgxpool.New(ctx, os.Getenv("NEON_V2_DATABASE_URL"))
	if err != nil {
		t.Fatal("metadata connection unavailable")
	}
	defer db.Close()
	var tenant, sourceTimeline, timeline, point, orgName string
	var proofJSON []byte
	var safe bool
	err = db.QueryRow(ctx, `SELECT pr.tenant_id,b.timeline_id,child.timeline_id,child.parent_lsn,org.name,r.proof,
 pr.source='managed' AND pr.state='ready' AND pr.deleted_at IS NULL AND org.state='active'
 AND b.state='ready' AND b.deleted_at IS NULL AND e.state IN ('active','suspended') AND e.deleted_at IS NULL
 AND child.state='ready' AND child.deleted_at IS NULL AND child.parent_branch_id=b.id
 AND NOT EXISTS(SELECT 1 FROM endpoints other WHERE other.project_id=pr.id AND other.branch_id IN (b.id,child.id) AND other.deleted_at IS NULL AND other.state NOT IN ('active','suspended'))
 AND NOT EXISTS(SELECT 1 FROM endpoints other WHERE other.branch_id=child.id)
 AND NOT EXISTS(SELECT 1 FROM endpoints other WHERE other.branch_id=b.id AND other.id<>e.id AND other.deleted_at IS NULL)
 AND o.action='create_branch' AND o.resource_id=child.id AND o.project_id=pr.id AND o.state='succeeded'
 AND o.payload->>'parent_timeline_id'=b.timeline_id AND o.payload->>'timeline_id'=child.timeline_id
 AND o.payload->>'parent_lsn'=child.parent_lsn
 FROM projects pr JOIN organizations org ON org.id=pr.org_id
 JOIN branches b ON b.project_id=pr.id AND b.id=$2
 JOIN endpoints e ON e.project_id=pr.id AND e.branch_id=b.id AND e.id=$3 AND e.endpoint_type='read_write'
 JOIN compute_catalog_bootstrap_receipts r ON r.endpoint_id=e.id
 JOIN compute_catalog_bootstrap_publications pub ON pub.endpoint_id=e.id
 JOIN branches child ON child.project_id=pr.id AND child.id=$4
 JOIN operations o ON o.id=$5
 WHERE pr.id=$1`, f.Project, f.SourceBranch, f.SourceEndpoint, f.RetainedBranch, f.OriginalOperation).Scan(&tenant, &sourceTimeline, &timeline, &point, &orgName, &proofJSON, &safe)
	if err != nil || !safe || !strings.HasPrefix(orgName, "ci-catalog-platform-") {
		t.Fatal("retained owned fork and dormant original source required")
	}
	var original computeCatalogBootstrap
	if json.Unmarshal(proofJSON, &original) != nil || original.Project != f.Project || original.Branch != f.SourceBranch || original.Endpoint != f.SourceEndpoint || original.Tenant != tenant || original.Timeline != sourceTimeline {
		t.Fatal("original published source witness rejected")
	}
	k, err := newKubeClient()
	if err != nil {
		t.Fatal("Kubernetes client unavailable")
	}
	if err = k.observeSuspendedComputeAbsence(ctx, suspendPayload{ProjectID: f.Project, EndpointID: f.SourceEndpoint, WorkloadName: kubeName(f.SourceEndpoint)}, 10*time.Second, 250*time.Millisecond); err != nil {
		t.Fatal("original source is not fully absent")
	}
	_, _, live, err := k.readComputeCatalogBootstrap(ctx, original.payload())
	if err != nil || live.ConfigUID != original.ConfigUID || live.CredentialUID != original.CredentialUID {
		t.Fatal("original sealed source identity rejected")
	}
	sourceCredential, err := k.request(ctx, http.MethodGet, k.path("secret", kubeName(f.SourceEndpoint)+"-credentials"), nil)
	if err != nil {
		t.Fatal("original source credential unavailable")
	}
	sourceConfig, err := k.request(ctx, http.MethodGet, k.path("secret", kubeName(f.SourceEndpoint)+"-config"), nil)
	if err != nil {
		t.Fatal("original source config unavailable")
	}
	originalData, err := resetSecretData(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	originalCredentials, err := resetSecretData(sourceCredential)
	if err != nil {
		t.Fatal(err)
	}
	sourceText, err := secretText(sourceConfig, "config.json")
	if err != nil {
		t.Fatal("original config unreadable")
	}
	var sourceDoc map[string]any
	if json.Unmarshal([]byte(sourceText), &sourceDoc) != nil {
		t.Fatal("original config invalid")
	}
	targetDoc, err := branchResetComputeConfig(sourceDoc, original, catalogNativeScope{tenant, timeline})
	if err != nil {
		t.Fatal("source cannot form protected target")
	}
	roots, err := branchResetConfigRootRoles(targetDoc)
	if err != nil {
		t.Fatal(err)
	}
	password, err := secretText(sourceCredential, "probePassword")
	if err != nil {
		t.Fatal("original probe unavailable")
	}
	window, err := k.timelineRestoreWindow(ctx, tenant, timeline)
	if err != nil || window.LatestLSN != point {
		t.Fatal("retained fork changed since its accepted original point")
	}
	info, err := k.serviceRequest(ctx, "pageserver-managed", 9898, "v1/tenant/"+tenant+"/timeline/"+timeline, http.MethodGet, nil)
	if err != nil || validateTimelineAncestor(info, sourceTimeline, point) != nil {
		t.Fatal("retained native fork ancestry changed")
	}
	routes, err := k.request(ctx, http.MethodGet, k.path("secret", routesSecret), nil)
	if err != nil {
		t.Fatal("original route registry unavailable")
	}
	registryUID := stringVal(nested(routes, "metadata", "uid"))
	sourceFiles, _ := json.Marshal([]any{originalData, originalCredentials})
	private := map[string]any{"source_files_sha256": catalogMarkerDigest(string(sourceFiles)), "project": f.Project, "branch": f.RetainedBranch, "operation": f.OriginalOperation, "registry_uid": registryUID, "native_timeline": timeline, "native_point": point}
	summary := map[string]any{"result": "running", "source_config_uid": original.ConfigUID, "source_credential_uid": original.CredentialUID, "retained_branch": f.RetainedBranch, "original_operation": f.OriginalOperation, "checks": []string{}}
	save := func() {
		t.Helper()
		a, _ := json.Marshal(private)
		b, _ := json.Marshal(summary)
		if os.WriteFile(filepath.Join(f.Output, "checkpoint.private.json"), a, 0600) != nil || os.WriteFile(filepath.Join(f.Output, "result.public.json"), b, 0600) != nil {
			t.Fatal("private Linux receipt unavailable")
		}
	}
	save()
	p := creationPayload(f.Project, f.RetainedBranch, branchResetBackupProbeEndpoint(f.OriginalOperation), tenant, timeline, createBounds{MinCPU: 1000, MaxCPU: 1000, MinMem: 1024, MaxMem: 1024})
	p.EndpointType, p.CatalogPolicy, p.ResetInspectionOperation, p.ResetInspectionLSN = "read_only", catalogPreserveInheritedSQL, f.OriginalOperation, point
	private["endpoint"] = p.EndpointID
	cred, err := k.createOwned(ctx, "secret", map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": map[string]any{"name": kubeName(p.EndpointID) + "-credentials", "labels": credentialLabels(f.Project, p.EndpointID)}, "data": map[string]string{"adminVerifier": base64.StdEncoding.EncodeToString([]byte(roots["cloud_admin"])), "probeVerifier": base64.StdEncoding.EncodeToString([]byte(roots["control_probe"])), "probePassword": base64.StdEncoding.EncodeToString([]byte(password))}}, f.Project, p.EndpointID)
	if err != nil {
		t.Fatal("private inspection credential reservation failed")
	}
	private["credential_uid"] = nested(cred, "metadata", "uid")
	save()
	var vmUID, serviceUID, alias string
	var route map[string]any
	cleanup := func() error {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		if alias != "" {
			if e := k.casBranchResetRoute(cleanupCtx, registryUID, alias, []map[string]any{route}, nil); e != nil {
				return e
			}
		}
		if vmUID != "" {
			current, e := k.request(cleanupCtx, http.MethodGet, k.path("vm", kubeName(p.EndpointID)), nil)
			var absent kubeError
			if e == nil {
				if !owned(current, p.ProjectID, p.EndpointID) || nested(current, "metadata", "uid") != vmUID {
					return errBranchResetProbe
				}
				if _, e = k.request(cleanupCtx, http.MethodDelete, k.path("vm", kubeName(p.EndpointID)), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": vmUID}}); e != nil {
					return e
				}
			} else if !errors.As(e, &absent) || absent.Status != 404 {
				return e
			}
			if e = k.waitVMGenerationDeletion(cleanupCtx, suspendPayload{ProjectID: p.ProjectID, EndpointID: p.EndpointID, WorkloadName: kubeName(p.EndpointID)}, vmUID, 90*time.Second, time.Second); e != nil {
				return e
			}
			if e = k.observeSuspendedComputeAbsence(cleanupCtx, suspendPayload{ProjectID: p.ProjectID, EndpointID: p.EndpointID, WorkloadName: kubeName(p.EndpointID)}, 90*time.Second, time.Second); e != nil {
				return e
			}
			vmUID = ""
		}
		if serviceUID != "" {
			current, e := k.request(cleanupCtx, http.MethodGet, k.path("service", kubeName(p.EndpointID)), nil)
			var absent kubeError
			if e == nil {
				if !owned(current, p.ProjectID, p.EndpointID) || nested(current, "metadata", "uid") != serviceUID {
					return errBranchResetProbe
				}
				if _, e = k.request(cleanupCtx, http.MethodDelete, k.path("service", kubeName(p.EndpointID)), map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": serviceUID}}); e != nil {
					return e
				}
			} else if !errors.As(e, &absent) || absent.Status != 404 {
				return e
			}
			serviceUID = ""
		}
		return nil
	}
	defer func() {
		if e := cleanup(); e != nil {
			t.Error("preserve private checkpoint; fixed-UID inspection cleanup incomplete")
		}
	}()
	created, err := k.createCompute(ctx, p)
	if created != nil {
		vmUID = stringVal(nested(created, "vm", "metadata", "uid"))
		private["vm_uid"] = vmUID
		save()
	}
	if err != nil {
		t.Fatal("private Static inspection VM creation failed")
	}
	service, err := k.request(ctx, http.MethodGet, k.path("service", kubeName(p.EndpointID)), nil)
	if err != nil || !owned(service, p.ProjectID, p.EndpointID) {
		t.Fatal("private Service unavailable")
	}
	serviceUID = stringVal(nested(service, "metadata", "uid"))
	private["service_uid"] = serviceUID
	save()
	vm, err := k.waitCompute(ctx, p.ProjectID, p.EndpointID)
	if err != nil || nested(vm, "metadata", "uid") != vmUID {
		t.Fatal("original private VM not Running")
	}
	alias, route, err = makeBranchResetBackupProbeRoute(p.ResetInspectionOperation, p.ProjectID, p.BranchID, p.TenantID, k.namespace, roots["control_probe"], f.CIDRs)
	if err != nil {
		t.Fatal("private backup route rejected")
	}
	private["alias"], private["route_sha256"] = alias, branchResetConfigDigest(route)
	save()
	if err = k.casBranchResetRoute(ctx, registryUID, alias, []map[string]any{nil}, route); err != nil {
		t.Fatal("private backup route reservation failed")
	}
	var conn *pgx.Conn
	for attempt := 0; attempt < 20; attempt++ {
		conn, err = connectSQL(ctx, env("NEON_PROXY_HOST", "192.168.146.100"), env("NEON_PROXY_PORT", "30432"), "control_probe", password, "template1", alias)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		t.Fatal("private Proxy Static SQL not ready")
	}
	var recovery bool
	var readOnly string
	if err = conn.QueryRow(ctx, `SELECT pg_is_in_recovery(),current_setting('transaction_read_only')`).Scan(&recovery, &readOnly); err != nil || !recovery || readOnly != "on" {
		conn.Close(ctx)
		t.Fatal("native backup is not in read-only recovery")
	}
	if _, err = readCatalogNativeSnapshot(ctx, conn, catalogNativeScope{tenant, timeline}); err != nil {
		conn.Close(ctx)
		t.Fatal("native backup scope rejected")
	}
	if _, err = conn.Exec(ctx, "BEGIN READ WRITE"); err == nil {
		conn.Exec(ctx, "ROLLBACK")
		conn.Close(ctx)
		t.Fatal("Static backup accepted a write transaction")
	}
	conn.Close(ctx)
	data, err := connectSQL(ctx, env("NEON_PROXY_HOST", "192.168.146.100"), env("NEON_PROXY_PORT", "30432"), "control_probe", password, f.Database, alias)
	if err != nil {
		t.Fatal("original small business database unavailable")
	}
	var rows int
	err = data.QueryRow(ctx, `SELECT count(*) FROM app.function_probe WHERE marker='original-canary'`).Scan(&rows)
	data.Close(ctx)
	if err != nil || rows != 1 {
		t.Fatal("original small business row not retained")
	}
	summary["checks"] = []string{"real_static_native_recovery_and_readonly", "private_proxy_selector_tls_and_original_scram", "original_small_business_row_retained", "write_transaction_rejected"}
	save()
	if err = cleanup(); err != nil {
		t.Fatal("private VM cleanup did not retain exact scope")
	}
	after, err := k.timelineRestoreWindow(ctx, tenant, timeline)
	if err != nil || after.LatestLSN != window.LatestLSN {
		t.Fatal("read-only inspection advanced retained native timeline")
	}
	finalConfig, e := k.request(ctx, http.MethodGet, k.path("secret", kubeName(f.SourceEndpoint)+"-config"), nil)
	if e != nil || nested(finalConfig, "metadata", "uid") != original.ConfigUID {
		t.Fatal("original source config identity changed")
	}
	finalCredential, e := k.request(ctx, http.MethodGet, k.path("secret", kubeName(f.SourceEndpoint)+"-credentials"), nil)
	if e != nil || nested(finalCredential, "metadata", "uid") != original.CredentialUID {
		t.Fatal("original source credential identity changed")
	}
	a, _ := resetSecretData(finalConfig)
	b, _ := resetSecretData(finalCredential)
	beforeFiles, _ := json.Marshal([]any{originalData, originalCredentials})
	afterFiles, _ := json.Marshal([]any{a, b})
	if string(beforeFiles) != string(afterFiles) {
		t.Fatal("original complete private files changed")
	}
	if err = k.observeSuspendedComputeAbsence(ctx, suspendPayload{ProjectID: f.Project, EndpointID: f.SourceEndpoint, WorkloadName: kubeName(f.SourceEndpoint)}, 10*time.Second, 250*time.Millisecond); err != nil {
		t.Fatal("original public Endpoint was started")
	}
	summary["result"], summary["checks"] = "pass", []string{"real_static_native_recovery_and_readonly", "private_proxy_selector_tls_and_original_scram", "original_small_business_row_retained", "write_transaction_rejected", "retained_timeline_head_unchanged", "original_public_workload_absent_and_complete_private_files_retained", "private_alias_vm_and_service_retired_by_original_identity"}
	save()
}
