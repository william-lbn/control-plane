package control

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/william-lbn/control-plane/api/internal/adapter"
)

// 该回归调用现有唯一 publisher 和原 Kube Fake。暂停期间的角色变更尚未
// 在新 Reader 上核验时，新 binding 的任何一次 CAS 都不能携带旧普通凭据。
func TestDSGAReaderResumeCutoverDoesNotPublishStaleRoles(t *testing.T) {
	f, old, before := managedSQLReplacementFixture(t)
	old.ServiceUID = "original-retired-service"
	f.mutate(func(objects map[string]map[string]any) {
		config := objects[f.k.path("secret", "cp-test-config")]
		raw, err := secretText(config, "config.json")
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if json.Unmarshal([]byte(raw), &doc) != nil {
			t.Fatal("invalid fixture config")
		}
		doc["spec"].(map[string]any)["mode"] = "Replica"
		encoded, _ := json.Marshal(doc)
		config["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(encoded)
		f.in.Binding.ConfigSHA256 = managedSQLDigest(encoded)
		f.in.Binding.SecretDataSHA256 = managedSQLDataDigest(map[string][]byte{"config.json": encoded})
		registry := objects[f.k.path("secret", routesSecret)]
		raw, _ = secretText(registry, "routes.json")
		var routes map[string]map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &routes)
		routes["ep-test"]["managed_binding"], _ = json.Marshal(old)
		routes["ep-test"]["state"] = json.RawMessage(`"active"`)
		var previousRoles map[string]string
		_ = json.Unmarshal(routes["ep-test"]["roles"], &previousRoles)
		previousRoles["retired_customer"] = previousRoles["custom_frozen_role"]
		routes["ep-test"]["roles"], _ = json.Marshal(previousRoles)
		encoded, _ = json.Marshal(routes)
		registry["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(encoded)
		objects[f.k.path("pod", "")] = managedSQLTestPodList()
	})
	f.in.resume = &managedSQLResumeProof{
		OriginalVMUID: old.VMUID, OriginalConfigUID: old.ConfigSecretUID, OriginalServiceUID: old.ServiceUID,
		ReaderCatalog: true,
		Verify:        func(context.Context, adapter.ManagedSQLBinding) error { return nil },
	}
	if _, err := publishManagedSQLRoute(context.Background(), f.k, f.in, f.observe, nil); err != nil {
		t.Fatal("original Reader resume rejected", err)
	}
	got := readerCatalogRoute(t, f)
	roles := got["roles"].(map[string]any)
	if len(roles) != 2 || roles["app_owner"] == nil || roles["control_probe"] == nil {
		t.Fatal("new Reader binding published an ordinary verifier from the retired generation")
	}
	var policy any
	if json.Unmarshal(before["network_acl"], &policy) != nil {
		t.Fatal("invalid original policy")
	}
	want, _ := json.Marshal(policy)
	actual, _ := json.Marshal(got["network_acl"])
	if string(want) != string(actual) {
		t.Fatal("Reader resume changed network policy")
	}
	// 已核原生事实之后才允许精确补入新角色及旋转凭据；删除的旧角色保持
	// 不存在。模拟租约/最终证明失败时不发布；相同最终清单重放不追加 PUT。
	fresh, _ := scramVerifier("current-resume-credential")
	credentials := map[string]string{"new_customer": fresh, "custom_frozen_role": fresh}
	if err := publishDSGAReaderRoles(context.Background(), f.k, f.in, credentials, func(context.Context) error { return errLeaseLost }); err == nil {
		t.Fatal("unconfirmed final proof published ordinary roles")
	}
	if len(readerCatalogRoute(t, f)["roles"].(map[string]any)) != 2 {
		t.Fatal("failed final proof restored stale credentials")
	}
	if err := publishDSGAReaderRoles(context.Background(), f.k, f.in, credentials, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	current := readerCatalogRoute(t, f)["roles"].(map[string]any)
	if len(current) != 4 || current["custom_frozen_role"] != fresh || current["new_customer"] != fresh || current["retired_customer"] != nil {
		t.Fatal("current whitelist retained an old or missing role")
	}
	writes, _ := f.counts()
	if err := publishDSGAReaderRoles(context.Background(), f.k, f.in, credentials, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if after, _ := f.counts(); after != writes {
		t.Fatal("proven whitelist replay repeated a write")
	}
}

func TestDSGAReaderResumeRequiresSealedReplicaMode(t *testing.T) {
	for _, mode := range []string{"", "Primary", "Unknown"} {
		t.Run("mode_"+mode, func(t *testing.T) {
			f, old, _ := managedSQLReplacementFixture(t)
			f.mutate(func(objects map[string]map[string]any) {
				config := objects[f.k.path("secret", "cp-test-config")]
				raw, _ := secretText(config, "config.json")
				var doc map[string]any
				_ = json.Unmarshal([]byte(raw), &doc)
				if mode != "" {
					doc["spec"].(map[string]any)["mode"] = mode
				}
				encoded, _ := json.Marshal(doc)
				config["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(encoded)
				f.in.Binding.ConfigSHA256 = managedSQLDigest(encoded)
				f.in.Binding.SecretDataSHA256 = managedSQLDataDigest(map[string][]byte{"config.json": encoded})
			})
			f.in.resume = &managedSQLResumeProof{OriginalVMUID: old.VMUID, OriginalConfigUID: old.ConfigSecretUID, OriginalServiceUID: old.ServiceUID, ReaderCatalog: true, Verify: func(context.Context, adapter.ManagedSQLBinding) error { return nil }}
			if _, err := publishManagedSQLRoute(context.Background(), f.k, f.in, f.observe, nil); ManagedSQLRouteFailureCode(err) != "managed_sql_route_source" {
				t.Fatal("Reader accepted unconfirmed mode", err)
			}
			if writes, _ := f.counts(); writes != 0 {
				t.Fatal("missing Reader mode changed the original route")
			}
		})
	}
}

func TestDSGAReaderResumeRefreshPinsCurrentBootAndOriginalMutation(t *testing.T) {
	for _, action := range []string{"create_role", "rotate_role_password", "delete_role"} {
		p, item, _ := resumeMutationSample(action)
		instance := dsgapi.Instance{VMUID: "new-reader-vm", ComputeInstanceID: "reader-compute", PGBootID: 92}
		verifier := dsgaRenderFixture().ApplicationVerifier
		input, err := readerResumeRefreshInput(p, item, verifier, instance)
		if err != nil || input.CatalogRole.OperationID != item.Operation || input.CatalogRole.ExpectedOID != item.Role.Ref.OID ||
			input.Generation != dsgapi.Uint64(item.Payload.ExpectedVersion) || input.PGBootID != instance.PGBootID ||
			input.EndpointID != p.EndpointID || (input.Verifier == nil) != (action == "delete_role") {
			t.Fatal("resume signed the wrong original mutation or boot", err)
		}
		for _, scenario := range []string{"exact", "old_boot", "other_oid", "other_operation", "other_timeline", "generation"} {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					var got dsgaAuthLoginRequest
					if dsgapi.UnmarshalObject(raw, &got) != nil || !reflect.DeepEqual(got, input) {
						t.Error("request differs from frozen proof")
						w.WriteHeader(400)
						return
					}
					out := dsgaAuthLoginReceipt{ProtocolVersion: 1, Service: input.Service, BranchID: input.BranchID, TenantID: input.TenantID, TimelineID: input.TimelineID,
						PGBootID: input.PGBootID, Generation: input.Generation, Enabled: input.Verifier != nil,
						CatalogRole: &dsgaCatalogRoleReceipt{OperationID: input.CatalogRole.OperationID, Action: input.CatalogRole.Action, OID: input.CatalogRole.ExpectedOID}}
					switch scenario {
					case "old_boot":
						out.PGBootID--
					case "other_oid":
						out.CatalogRole.OID++
					case "other_operation":
						out.CatalogRole.OperationID = "op_222222222222222222222222"
					case "other_timeline":
						out.TimelineID = strings.Repeat("f", 32)
					case "generation":
						out.Generation++
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Cache-Control", "no-store")
					_ = json.NewEncoder(w).Encode(out)
				}))
				defer peer.Close()
				key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
				_, e := requestDSGAAuthLogin(context.Background(), peer.Client(), peer.URL, key, &dsgaEndpointRuntime{Payload: p, VMUID: instance.VMUID}, input)
				if (e == nil) != (scenario == "exact") {
					t.Fatal("resume accepted foreign receipt", e)
				}
			})
		}
		instance.PGBootID = 0
		if _, err = readerResumeRefreshInput(p, item, verifier, instance); err == nil {
			t.Fatal("resume inferred a missing current PG boot")
		}
	}
}

func resumeMutationSample(action string) (createPayload, dsgaReaderResumeRole, []byte) {
	p, roles, _ := readerCatalogSample()
	p.EndpointID = "ep_0123456789abcdef"
	ref := roles[0].Ref
	v := catalogPayload{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID, Name: roles[0].Name,
		ExpectedVersion: ref.Version - 1, CredentialRef: ref.CredentialRef, MutationCredentialUID: ref.CredentialUID,
		NativeIdentity: &catalogMutationIdentity{Scope: catalogNativeScope{p.TenantID, p.TimelineID}}}
	item := dsgaReaderResumeRole{Role: roles[0], State: "ready", SourceType: "create_result", Operation: "op_111111111111111111111111", Action: action}
	if action != "create_role" {
		item.SourceType = "mutation_result"
		v.NativeIdentity.Object = ref
		v.NativeIdentity.Object.ID = "cob_previous"
		v.NativeIdentity.Object.Version--
	}
	if action == "delete_role" {
		item.State = "deleted"
		v.CredentialRef = ""
		v.MutationCredentialUID = ""
	}
	item.Payload = v
	raw, _ := json.Marshal(map[string]string{"name": item.Role.Name, "kind": "role", "state": item.State, "version_token": strconv.FormatInt(ref.Version, 10)})
	return p, item, raw
}

func TestDSGAReaderResumeMutationProof(t *testing.T) {
	for _, action := range []string{"create_role", "rotate_role_password", "delete_role"} {
		for _, scenario := range []string{"exact", "scope", "branch", "name", "generation", "credential_uid", "payload_digest", "result_version", "source_kind", "replacement_oid", "identity"} {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				p, item, result := resumeMutationSample(action)
				digest := catalogMutationDigest(action, item.Payload)
				want := scenario == "exact"
				switch scenario {
				case "scope":
					item.Payload.NativeIdentity.Scope.TimelineID = strings.Repeat("3", 32)
				case "branch":
					item.Payload.BranchID = "br_foreign"
				case "name":
					item.Payload.Name = "replaced"
				case "generation":
					item.Role.Ref.Version++
				case "credential_uid":
					item.Payload.MutationCredentialUID = "22222222-2222-4222-8222-222222222222"
					want = action == "delete_role"
				case "payload_digest":
					digest = strings.Repeat("0", 64)
				case "result_version":
					result = []byte(`{"name":"customer","kind":"role","state":"ready","version_token":"99"}`)
				case "source_kind":
					item.SourceType = "inherited_identity"
				case "replacement_oid":
					item.Payload.NativeIdentity.Object.OID++
					want = false
				case "identity":
					item.Payload.NativeIdentity.Object.IdentityID = "coi_foreign"
					want = false
				}
				// 单独核关系约束，摘要另有独立负控，不能由摘要失配掩盖检查缺口。
				if scenario != "payload_digest" {
					digest = catalogMutationDigest(action, item.Payload)
				}
				err := validateDSGAReaderResumeMutation(p, item, digest, result)
				if (err == nil) != want {
					t.Fatalf("allowed=%v err=%v", want, err)
				}
			})
		}
	}
}

// 真实 control_ci 元库使用生产提交函数固定 OID/credential UID，再读取恢复
// 所需证明。Kubernetes 仍为既有 Fake，不声称运行了 Guest 或真实 Reader SQL。
func TestDSGAReaderResumeMetadataUsesCommittedCurrentMutation(t *testing.T) {
	ctx, b, _ := dsgaSuspensionAdmissionFixture(t)
	s := b.server
	p := b.intent.createPayload
	p.EndpointType = "read_only"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('binding_admin','binding_admin','unused','owner') ON CONFLICT DO NOTHING`)
	load := func() ([]dsgaReaderResumeRole, int, error) {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return nil, 0, err
		}
		defer tx.Rollback(ctx)
		return readDSGAReaderResumeRoles(ctx, tx, p)
	}
	var prior catalogObjectRef
	for n, action := range []string{"create_role", "rotate_role_password", "delete_role"} {
		v := catalogPayload{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: b.intent.EndpointID, Name: "resume_customer", ExpectedVersion: int64(n + 1),
			NativeIdentity: &catalogMutationIdentity{Scope: catalogNativeScope{p.TenantID, p.TimelineID}, Object: prior}}
		state := "creating"
		if action == "rotate_role_password" {
			state = "updating"
		}
		if action == "delete_role" {
			state = "deleting"
		} else {
			v.CredentialRef = fmt.Sprintf("resume-credential-%d", n)
			v.MutationCredentialUID = fmt.Sprintf("%08d-1111-4111-8111-111111111111", n+1)
		}
		v.PreviousCredentialRef = prior.CredentialRef
		if n == 0 {
			exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,$2,$3,$4,1)`, p.BranchID, v.Name, state, v.CredentialRef)
		} else {
			exec(`UPDATE branch_roles SET state=$3 WHERE branch_id=$1 AND name=$2`, p.BranchID, v.Name, state)
		}
		v = bindingOperation(t, exec, action, v)
		if action == "create_role" {
			if err := s.recordCatalogCreateObservation(ctx, v, action, 16395); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := s.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = commitCatalogMutationResultTx(ctx, tx, v.operationID, v.workerID, action, v); err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		// 原 Operation 未完成时即使存在 result，也不能发布恢复凭据。
		if _, _, err = load(); err == nil {
			t.Fatal("uncommitted original operation was accepted")
		}
		exec(`UPDATE operations SET state='succeeded',finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, v.operationID)
		items, omitted, err := load()
		if err != nil || omitted != 0 || len(items) != 1 || items[0].Action != action || items[0].Role.Ref.OID != 16395 || items[0].Payload.ExpectedVersion != v.ExpectedVersion {
			t.Fatalf("current mutation lost: count=%d omitted=%d err=%v", len(items), omitted, err)
		}
		prior = items[0].Role.Ref
		if n == 1 && prior.CredentialUID != v.MutationCredentialUID {
			t.Fatal("resume kept old rotated credential UID")
		}
		if n == 2 && items[0].State != "deleted" {
			t.Fatal("delete tombstone lost")
		}
		again, omitted, err := load()
		if err != nil || omitted != 0 || !reflect.DeepEqual(items, again) {
			t.Fatal("read replay changed original proof", err)
		}
	}
	// 无当前原生 mutation 的继承/观察来源只影响对应帐号，不能阻断已证明
	// 的删除事实或把父分支历史物理角色自动纳入当前普通登录。
	for i, kind := range []string{"initial_observation", "inherited_identity", "current_observation", "rename_result"} {
		name := fmt.Sprintf("unproven_%d", i)
		exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref,version) VALUES($1,$2,'ready','old-credential',1)`, p.BranchID, name)
		tx, err := s.db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ref := catalogObjectRef{ID: fmt.Sprintf("cob_unproven_%d", i), IdentityID: fmt.Sprintf("coi_unproven_%d", i), OID: uint32(17000 + i), Version: 1, CredentialRef: "old-credential", CredentialUID: "11111111-1111-4111-8111-111111111111"}
		if err = insertCatalogObjectBinding(ctx, tx, p.ProjectID, p.BranchID, "role", name, "ready", kind, "old-proof", catalogNativeScope{p.TenantID, p.TimelineID}, ref); err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	items, omitted, err := load()
	if err != nil || omitted != 4 || len(items) != 1 || items[0].Action != "delete_role" {
		t.Fatal("unproven identities blocked or joined proven roles", err)
	}
	// 关联 Timeline 改变必须拒绝有原生来源的记录，不能降格为可忽略旧数据。
	p.TimelineID = strings.Repeat("f", 32)
	if _, _, err = load(); err == nil {
		t.Fatal("foreign timeline adopted current native mutation")
	}
}
