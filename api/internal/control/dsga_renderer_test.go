package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

// verifier只由固定测试字节构造，不含真实密码；跨语言导出也只输出这些样本。
func dsgaRenderFixture() dsgaRenderCredentials {
	verifier := "SCRAM-SHA-256$4096:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16)) + "$" +
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)) + ":" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, 32))
	return dsgaRenderCredentials{verifier, verifier, key.Public().(ed25519.PublicKey)}
}

func TestDSGARendererDeterministicPrimaryAndReplicaSafety(t *testing.T) {
	p, _, plan := dsgaPlanFixture(t)
	credentials := dsgaRenderFixture()
	for _, mode := range []string{"read_write", "read_only"} {
		t.Run(mode, func(t *testing.T) {
			p.EndpointType = mode
			a, err := renderDSGAComputeBootstrap(p, plan, "actual-vm-uid", credentials)
			if err != nil {
				t.Fatal(err)
			}
			b, err := renderDSGAComputeBootstrap(p, plan, "actual-vm-uid", credentials)
			if err != nil || !bytes.Equal(a, b) {
				t.Fatal("same original inputs did not produce identical configuration")
			}
			var result struct {
				Spec struct {
					Operation string              `json:"operation_uuid"`
					Timestamp string              `json:"timestamp"`
					Native    dsgaNativeBootstrap `json:"dynamic_shared_buffers"`
					Mode      string              `json:"mode"`
					Cluster   struct {
						Roles    []dsgaRenderRole   `json:"roles"`
						Settings []dsgaRenderOption `json:"settings"`
					} `json:"cluster"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(a, &result); err != nil {
				t.Fatal(err)
			}
			if result.Spec.Native.dsgaBootstrapBudget != plan.Budget || result.Spec.Native.VMUID != "actual-vm-uid" || result.Spec.Operation != plan.OperationUUID || result.Spec.Timestamp != plan.AcceptedAt {
				t.Fatal("render lost original identity/time or integer budget")
			}
			settings := map[string]string{}
			for _, option := range result.Spec.Cluster.Settings {
				if option.Value != nil {
					settings[option.Name] = *option.Value
				}
			}
			if settings["fsync"] != "on" || settings["password_encryption"] != "scram-sha-256" || settings["shared_preload_libraries"] != "neon,pg_stat_statements" {
				t.Fatal("inherited unsafe lab configuration or unaccounted preload")
			}
			if (mode == "read_only") != (result.Spec.Mode == "Replica") || (mode == "read_only") != (settings["primary_conninfo"] != "") {
				t.Fatal("replica missing actual safekeeper recovery configuration")
			}
			if len(result.Spec.Cluster.Roles) != 2 || result.Spec.Cluster.Roles[0].Name != dsgaApplicationRole || result.Spec.Cluster.Roles[1].Name != "control_probe" {
				t.Fatal("internal administrator entered application roles")
			}
			for _, role := range result.Spec.Cluster.Roles {
				flags := map[string]bool{}
				for _, option := range role.Options {
					flags[option.Name] = true
				}
				for _, flag := range []string{"NOSUPERUSER", "NOCREATEROLE", "NOREPLICATION", "NOBYPASSRLS"} {
					if !flags[flag] {
						t.Fatal("implicit role privilege default not overridden")
					}
				}
			}
		})
	}
}

func TestDSGARendererRejectsUnsafeInputsWithoutPartialOutput(t *testing.T) {
	p, _, plan := dsgaPlanFixture(t)
	for name, mutate := range map[string]func(*createPayload, *dsgaCreationPlan, *dsgaRenderCredentials, *string){
		"pg_version": func(_ *createPayload, plan *dsgaCreationPlan, _ *dsgaRenderCredentials, _ *string) {
			plan.PostgresVersion = 16
		},
		"budget": func(_ *createPayload, plan *dsgaCreationPlan, _ *dsgaRenderCredentials, _ *string) {
			plan.Budget.NonBufferReserveBytes = 0
		},
		"vm": func(_ *createPayload, _ *dsgaCreationPlan, _ *dsgaRenderCredentials, vm *string) { *vm = "" },
		"owner_md5": func(_ *createPayload, _ *dsgaCreationPlan, c *dsgaRenderCredentials, _ *string) {
			c.ApplicationVerifier = "md5old"
		},
		"probe_plaintext": func(_ *createPayload, _ *dsgaCreationPlan, c *dsgaRenderCredentials, _ *string) {
			c.ProbeVerifier = "plaintext-not-a-verifier"
		},
		"no_public_key": func(_ *createPayload, _ *dsgaCreationPlan, c *dsgaRenderCredentials, _ *string) {
			c.ControlPublicKey = nil
		},
		"zero_public_key": func(_ *createPayload, _ *dsgaCreationPlan, c *dsgaRenderCredentials, _ *string) {
			c.ControlPublicKey = make([]byte, 32)
		},
		"catalog_sql": func(p *createPayload, _ *dsgaCreationPlan, _ *dsgaRenderCredentials, _ *string) {
			p.CatalogSpec = `{"roles":[{"name":"cloud_admin"}]}`
		},
		"catalog_policy": func(p *createPayload, _ *dsgaCreationPlan, _ *dsgaRenderCredentials, _ *string) {
			p.CatalogPolicy = catalogPreserveInheritedSQL
		},
	} {
		t.Run(name, func(t *testing.T) {
			q, frozen, c, vm := p, plan, dsgaRenderFixture(), "actual-vm-uid"
			mutate(&q, &frozen, &c, &vm)
			out, err := renderDSGAComputeBootstrap(q, frozen, vm, c)
			if out != nil || !errors.Is(err, errDSGARender) {
				t.Fatal("unsafe input produced a partial usable config")
			}
		})
	}
}

func TestDSGARendererUsesOriginalUIDAndImmutableCreationBarrier(t *testing.T) {
	f, input := newCreationTest(t)
	p, _, plan := dsgaPlanFixture(t)
	p.ProjectID, p.EndpointID = input.Payload.ProjectID, input.Payload.EndpointID
	plan.Namespace = f.kube.namespace
	plan.Incarnation, plan.ComputeInstanceID = input.VM.Spec.DSGABootstrap.Incarnation, input.VM.Spec.DSGABootstrap.ComputeInstanceID
	input.Payload = p
	input.Render = func(_ context.Context, vm *vmv1.VirtualMachine) (map[string][]byte, error) {
		f.mu.Lock()
		bound := f.bound == vm.UID && dsgapi.Managed(vm.Annotations)
		f.mu.Unlock()
		if !bound {
			return nil, errDSGACreationIdentity
		}
		out, err := renderDSGAComputeBootstrap(p, plan, string(vm.UID), dsgaRenderFixture())
		if err != nil {
			return nil, err
		}
		return map[string][]byte{"config.json": out}, nil
	}
	vm, err := f.kube.prepareDSGACreation(context.Background(), input)
	if err != nil || vm == nil || !vm.Spec.DSGABootstrap.Ready {
		t.Fatalf("creation barrier failed: %v", err)
	}
	f.mu.Lock()
	before := append([]byte(nil), f.secret.Data["config.json"]...)
	immutable := f.secret.Immutable != nil && *f.secret.Immutable && f.secret.OwnerReferences[0].UID == vm.UID
	trace := append([]string(nil), f.trace...)
	f.mu.Unlock()
	if !immutable || !reflect.DeepEqual(trace, []string{"create", "bind", "initialize", "config", "bind-config", "release"}) {
		t.Fatal("renderer bypassed original UID/journal/immutable configuration")
	}
	input.ExpectedVMUID, input.ExpectedConfigUID = vm.UID, "config-uid"
	if _, err := f.kube.prepareDSGACreation(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !bytes.Equal(before, f.secret.Data["config.json"]) || !reflect.DeepEqual(trace, f.trace) {
		t.Fatal("recovery rewrote config or repeated original creation")
	}
}

func TestDSGARendererNativeContractExport(t *testing.T) {
	path := os.Getenv("NEON_DSGA_RENDERER_CONTRACT")
	if path == "" {
		t.Skip("explicit fresh Go-to-Rust renderer contract path required")
	}
	type sample struct {
		Name   string          `json:"name"`
		Config json.RawMessage `json:"config"`
	}
	p, _, plan := dsgaPlanFixture(t)
	var samples []sample
	for _, mode := range []string{"read_write", "read_only"} {
		p.EndpointType = mode
		data, err := renderDSGAComputeBootstrap(p, plan, "actual-vm-uid", dsgaRenderFixture())
		if err != nil {
			t.Fatal(err)
		}
		samples = append(samples, sample{mode, data})
	}
	data, err := json.Marshal(samples)
	if err != nil {
		t.Fatal(err)
	}
	// 拒绝覆盖旧文件，零用例或没运行导出器不能靠历史文件假装互通。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
