package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readerCatalogSample() (createPayload, []dsgaReaderRole, catalogNativeSnapshot) {
	p := createPayload{ProjectID: "prj_test", BranchID: "br_test", EndpointID: "ep_test", EndpointType: "read_only", TenantID: strings.Repeat("1", 32), TimelineID: strings.Repeat("2", 32)}
	roles := []dsgaReaderRole{{Name: "customer", Ref: catalogObjectRef{ID: "cob_original", IdentityID: "coi_original", OID: 16395, Version: 3, CredentialRef: "catalog-original", CredentialUID: "11111111-1111-4111-8111-111111111111"}}}
	snapshot := catalogNativeSnapshot{Scope: catalogNativeScope{p.TenantID, p.TimelineID}, Roles: []catalogNativeRole{{OID: 16395, Name: "customer", Marker: "neon-control/branch/" + p.BranchID, Flags: catalogRoleFlags{Login: true}}}, Databases: []catalogNativeDatabase{}}
	return p, roles, snapshot
}

func TestDSGAReaderCatalogOriginalIdentity(t *testing.T) {
	for _, scenario := range []string{"exact", "unmanaged_extra", "wrong_timeline", "wrong_branch", "replacement_oid", "renamed", "missing", "nologin", "superuser", "createrole", "createdb", "replication", "bypassrls", "duplicate", "reserved", "writer"} {
		t.Run(scenario, func(t *testing.T) {
			p, roles, snapshot := readerCatalogSample()
			switch scenario {
			case "unmanaged_extra":
				snapshot.Roles = append(snapshot.Roles, catalogNativeRole{OID: 16396, Name: "historical_parent_role", Marker: "neon-control/branch/br_parent", Flags: catalogRoleFlags{Login: true}})
			case "wrong_timeline":
				snapshot.Scope.TimelineID = strings.Repeat("3", 32)
			case "wrong_branch":
				snapshot.Roles[0].Marker = "neon-control/branch/br_parent"
			case "replacement_oid":
				snapshot.Roles[0].OID++
			case "renamed":
				snapshot.Roles[0].Name = "renamed"
			case "missing":
				snapshot.Roles = nil
			case "nologin":
				snapshot.Roles[0].Flags.Login = false
			case "superuser":
				snapshot.Roles[0].Flags.Superuser = true
			case "createrole":
				snapshot.Roles[0].Flags.CreateRole = true
			case "createdb":
				snapshot.Roles[0].Flags.CreateDB = true
			case "replication":
				snapshot.Roles[0].Flags.Replication = true
			case "bypassrls":
				snapshot.Roles[0].Flags.BypassRLS = true
			case "duplicate":
				roles = append(roles, roles[0])
			case "reserved":
				roles[0].Name, snapshot.Roles[0].Name = "app_owner", "app_owner"
			case "writer":
				p.EndpointType = "read_write"
			}
			before, _ := json.Marshal(roles)
			err := verifyDSGAReaderRoleSnapshot(p, roles, snapshot)
			want := scenario == "exact" || scenario == "unmanaged_extra"
			if (err == nil) != want {
				t.Fatalf("allowed=%v error=%v", want, err)
			}
			after, _ := json.Marshal(roles)
			if string(before) != string(after) || len(roles) == 0 {
				t.Fatal("observation changed the original authorization inventory")
			}
		})
	}
}

func readerCatalogRoute(t *testing.T, f *managedSQLTestFixture) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := secretText(f.objects[f.k.path("secret", routesSecret)], "routes.json")
	if err != nil {
		t.Fatal(err)
	}
	var routes map[string]map[string]any
	if json.Unmarshal([]byte(raw), &routes) != nil {
		t.Fatal("invalid fixture route")
	}
	return routes[selector(f.in.Binding.EndpointID)]
}

func mutateReaderCatalogRoute(t *testing.T, f *managedSQLTestFixture, mutate func(map[string]any)) {
	t.Helper()
	f.mutate(func(objects map[string]map[string]any) {
		registry := objects[f.k.path("secret", routesSecret)]
		raw, err := secretText(registry, "routes.json")
		if err != nil {
			t.Fatal(err)
		}
		var routes map[string]map[string]any
		if json.Unmarshal([]byte(raw), &routes) != nil {
			t.Fatal("invalid fixture route")
		}
		mutate(routes[selector(f.in.Binding.EndpointID)])
		encoded, _ := json.Marshal(routes)
		registry["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(encoded)
	})
}

func TestDSGAReaderCatalogRoutePublication(t *testing.T) {
	for _, scenario := range []string{"exact", "cas_retry", "no_base_route", "expired_authority", "wrong_branch", "replacement_vm", "changed_verifier", "unattested_role", "suspended", "registry_replaced", "source_changes_before_cas", "unknown_put"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f := newManagedSQLFixture(t)
			if scenario != "no_base_route" {
				if _, err := publishManagedSQLRoute(ctx, f.k, f.in, f.observe, nil); err != nil {
					t.Fatal(err)
				}
			}
			verifier, err := scramVerifier("reader-original-fixture")
			if err != nil {
				t.Fatal(err)
			}
			credentials := map[string]string{"customer": verifier}
			if scenario != "no_base_route" {
				mutateReaderCatalogRoute(t, f, func(route map[string]any) {
					route["allowed_ips"] = []any{}
					route["network_acl"] = map[string]any{"default": "deny"}
					switch scenario {
					case "wrong_branch":
						route["branch_id"] = "br_other"
					case "replacement_vm":
						route["managed_binding"].(map[string]any)["vm_uid"] = "foreign-vm"
					case "changed_verifier":
						route["roles"].(map[string]any)["customer"] = route["roles"].(map[string]any)["app_owner"]
					case "unattested_role":
						route["roles"].(map[string]any)["historical_parent_role"] = verifier
					case "suspended":
						route["state"] = "suspended"
					}
				})
			}
			before, _ := f.counts()
			if scenario == "registry_replaced" {
				f.mutate(func(objects map[string]map[string]any) {
					objects[f.k.path("secret", routesSecret)]["metadata"].(map[string]any)["uid"] = "foreign-registry"
				})
			}
			if scenario == "cas_retry" {
				f.conflicts = 1
			}
			if scenario == "unknown_put" {
				f.putFailure = 503
			}
			calls := 0
			authorize := func(context.Context) error {
				calls++
				if scenario == "expired_authority" {
					return errLeaseLost
				}
				if scenario == "source_changes_before_cas" {
					f.mutate(func(objects map[string]map[string]any) {
						objects[f.k.path("vm", kubeName(f.in.Binding.EndpointID))]["metadata"].(map[string]any)["uid"] = "foreign-vm"
					})
				}
				return nil
			}
			err = publishDSGAReaderRoles(ctx, f.k, f.in, credentials, authorize)
			want := scenario == "exact" || scenario == "cas_retry"
			if (err == nil) != want {
				t.Fatalf("allowed=%v error=%v", want, err)
			}
			after, _ := f.counts()
			if !want && scenario != "unknown_put" && before != after {
				t.Fatal("rejected observation mutated registry")
			}
			if want {
				route := readerCatalogRoute(t, f)
				if route["roles"].(map[string]any)["customer"] != verifier || !reflect.DeepEqual(route["allowed_ips"], []any{}) || route["network_acl"].(map[string]any)["default"] != "deny" {
					t.Fatal("original credential or deny policy was lost")
				}
				if err = publishDSGAReaderRoles(ctx, f.k, f.in, credentials, authorize); err != nil {
					t.Fatal(err)
				}
				idempotent, _ := f.counts()
				if idempotent != after || calls < 2 {
					t.Fatal("unchanged readback repeated a write or skipped authority verification")
				}
			}
		})
	}
}

func TestDSGAReaderCatalogCredentialIdentity(t *testing.T) {
	for _, scenario := range []string{"exact", "uid", "project", "mutable", "deleting", "name", "namespace", "missing", "invalid_verifier"} {
		t.Run(scenario, func(t *testing.T) {
			p, roles, _ := readerCatalogSample()
			f := newManagedSQLFixture(t)
			verifier, _ := scramVerifier("reader-credential-fixture")
			truth := true
			secret := &core.Secret{ObjectMeta: metav1.ObjectMeta{Name: roles[0].Ref.CredentialRef, Namespace: "testing", UID: "11111111-1111-4111-8111-111111111111", ResourceVersion: "1", Labels: credentialLabels(p.ProjectID, "ep_writer")}, Immutable: &truth, Type: core.SecretTypeOpaque, Data: map[string][]byte{"verifier": []byte(verifier)}}
			switch scenario {
			case "uid":
				secret.UID = "replacement"
			case "project":
				secret.Labels["neon-control/project-id"] = "prj_other"
			case "mutable":
				secret.Immutable = nil
			case "deleting":
				now := metav1.Now()
				secret.DeletionTimestamp = &now
			case "name":
				secret.Name = "replacement"
			case "namespace":
				secret.Namespace = "other"
			case "invalid_verifier":
				secret.Data["verifier"] = []byte("plain-password")
			}
			if scenario != "missing" {
				f.mutate(func(objects map[string]map[string]any) {
					objects[f.k.path("secret", roles[0].Ref.CredentialRef)] = managedSQLTestMap(t, secret)
				})
			}
			result, err := (&server{kube: f.k}).readDSGAReaderCredentials(context.Background(), p, roles)
			if (err == nil) != (scenario == "exact") || err == nil && result["customer"] != verifier {
				t.Fatal("original immutable credential verification differs", err)
			}
			if err != nil && (!errors.Is(err, errDSGAReaderCatalog) || strings.Contains(err.Error(), verifier)) {
				t.Fatal("credential error leaked source data")
			}
		})
	}
}

func TestDSGAReaderCatalogMetadataGeneration(t *testing.T) {
	ctx, s, _ := catalogBindingFixture(t)
	for _, scenario := range []string{"exact", "missing_binding", "old_generation", "foreign_scope", "wrong_credential", "deleted_binding", "pending_intent", "deleted_intent"} {
		t.Run(scenario, func(t *testing.T) {
			tx, err := s.db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			p, roles, _ := readerCatalogSample()
			p.ProjectID, p.BranchID, p.TenantID, p.TimelineID = "prj_0123456789abcdef", "br_0123456789abcdef", strings.Repeat("a", 32), strings.Repeat("b", 32)
			role := roles[0]
			state, boundState := "ready", "ready"
			if scenario == "pending_intent" {
				state = "creating"
			}
			if scenario == "deleted_intent" {
				state = "deleted"
			}
			if _, err = tx.Exec(ctx, `INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,$2,$3,$4,$5)`, p.BranchID, role.Name, state, role.Ref.CredentialRef, role.Ref.Version); err != nil {
				t.Fatal(err)
			}
			scope := catalogNativeScope{p.TenantID, p.TimelineID}
			switch scenario {
			case "old_generation":
				role.Ref.Version--
			case "foreign_scope":
				scope.TimelineID = strings.Repeat("c", 32)
			case "wrong_credential":
				role.Ref.CredentialRef = "other-credential"
			case "deleted_binding":
				boundState = "deleted"
			}
			if scenario != "missing_binding" {
				if err = insertCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, "role", role.Name, boundState, "create_result", "op_reader_fixture", scope, role.Ref); err != nil {
					t.Fatal(err)
				}
			}
			actual, err := readDSGAReaderRoles(ctx, tx, p)
			allowed := scenario == "exact" || scenario == "deleted_intent"
			if (err == nil) != allowed || scenario == "exact" && !reflect.DeepEqual(actual, roles) || scenario == "deleted_intent" && len(actual) != 0 {
				t.Fatalf("allowed=%v role count=%d err=%v", allowed, len(actual), err)
			}
			var count int
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM catalog_object_bindings WHERE project_id=$1 AND branch_id=$2`, p.ProjectID, p.BranchID).Scan(&count); err != nil || scenario == "missing_binding" && count != 0 {
				t.Fatal("read created an identity for the unbound intent", err)
			}
		})
	}
}

func TestDSGAReaderCatalogAdmissionRecognizesManagedCreate(t *testing.T) {
	ctx, binding, _, _, _ := dsgaServicesSourceFixture(t, "success")
	if binding.action != "create_endpoint_dsga_v1" {
		t.Fatal("fixture is not the real managed endpoint creation path")
	}
	p := binding.intent.createPayload
	pending, err := catalogMutationPending(ctx, binding.server.db, p.ProjectID, p.BranchID)
	if err != nil || !pending {
		t.Fatal("managed endpoint publication did not fence a new catalog admission", err)
	}
	pending, err = catalogMutationPending(ctx, binding.server.db, p.ProjectID, "br_unrelated")
	if err != nil || pending {
		t.Fatal("unrelated branch was incorrectly fenced", err)
	}
}

func TestDSGAReaderCatalogBranchLockKeepsLeaseLive(t *testing.T) {
	ctx, binding, _, _, _ := dsgaServicesSourceFixture(t, "success")
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	s := binding.server
	p := binding.intent.createPayload
	publication, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer publication.Rollback(context.Background())
	var branch string
	if err = publication.QueryRow(ctx, `SELECT id FROM branches WHERE id=$1 FOR UPDATE`, p.BranchID).Scan(&branch); err != nil {
		t.Fatal(err)
	}
	// 发布持有 branch 锁时，真实 Operation 续租及短期绑定复核都须完成。
	// 这覆盖错误地跨外部请求持有 Operation 行锁会造成的自锁/续租停滞。
	done := make(chan error, 1)
	go func() {
		_, e := s.db.Exec(ctx, `UPDATE operations SET lease_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1 AND state='running' AND lease_owner=$2`, binding.operationID, binding.workerID)
		if e == nil {
			_, e = s.loadDSGACreationBinding(ctx, binding.operationID, binding.workerID)
		}
		done <- e
	}()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("branch publication blocked the Operation heartbeat or source recheck")
	}
	blocked := make(chan error, 1)
	go func() {
		admission, e := s.db.Begin(ctx)
		if e != nil {
			blocked <- e
			return
		}
		defer admission.Rollback(context.Background())
		var current string
		e = admission.QueryRow(ctx, `SELECT id FROM branches WHERE id=$1 FOR UPDATE`, p.BranchID).Scan(&current)
		if e == nil {
			var pending bool
			pending, e = catalogMutationPending(ctx, admission, p.ProjectID, p.BranchID)
			if e == nil && !pending {
				e = errors.New("waiting admission lost the original creation fence")
			}
		}
		blocked <- e
	}()
	select {
	case err = <-blocked:
		t.Fatal("catalog admission crossed the held branch lock", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err = publication.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-blocked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("catalog admission did not resume after the branch lock was released")
	}
}
