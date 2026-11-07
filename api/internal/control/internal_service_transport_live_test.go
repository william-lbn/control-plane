package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

// 显式现场只读用例：受保护文件由运维读取原VM/Service/immutable配置后提供。
// 这里只请求初始化观察GET，不创建资源、不接管Owner、不授权SQL或执行内存动作。
// 不能用本用例替代公开API新建项目的端到端验收。
func TestInternalServiceTunnelLiveNativeReadOnly(t *testing.T) {
	path := os.Getenv("NEON_INTERNAL_NATIVE_READONLY_FIXTURE")
	if path == "" {
		t.Skip("explicit private live fixture not supplied")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("live fixture must be a regular private 0600 file")
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 16384 {
		t.Fatal("cannot read bounded private fixture")
	}
	var input struct {
		Workload          string          `json:"workload"`
		ComputeInstanceID string          `json:"compute_instance_id"`
		VMUID             string          `json:"vm_uid"`
		ServiceUID        string          `json:"service_uid"`
		ServiceIP         string          `json:"service_ip"`
		ConfigUID         string          `json:"config_uid"`
		TenantID          string          `json:"tenant_id"`
		TimelineID        string          `json:"timeline_id"`
		OperationUUID     string          `json:"operation_uuid"`
		Startup           json.RawMessage `json:"startup"`
		Seed              []byte          `json:"seed"`
	}
	if dsgapi.UnmarshalObject(raw, &input) != nil || len(input.Seed) != ed25519.SeedSize ||
		net.ParseIP(input.ServiceIP) == nil || dsgapi.ValidateID(input.VMUID, "vm") != nil ||
		dsgapi.ValidateID(input.ServiceUID, "service") != nil || dsgapi.ValidateID(input.ConfigUID, "config") != nil {
		t.Fatal("invalid private source snapshot")
	}
	wanted, err := decodeManagedSQLStartup(input.Startup)
	if err != nil || wanted.VMUID != input.VMUID || wanted.ComputeInstanceID != input.ComputeInstanceID {
		t.Fatal("original native startup identity mismatch")
	}
	via, err := internalServiceSOCKS5()
	if err != nil || via == "" {
		t.Fatal("explicit local SSH transport required")
	}
	key := ed25519.NewKeyFromSeed(input.Seed)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(input.ServiceIP, "3080")+"/dsg/v1/initialization", nil)
	if err != nil {
		t.Fatal("invalid original management destination")
	}
	request.Header.Set("Authorization", "Bearer "+dsgaInitializationJWT(key, input.Workload, input.VMUID, wanted.ComputeInstanceID, time.Now()))
	request.Header.Set("Cache-Control", "no-store")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	client := newDSGAProxy(nil).observer
	client.Timeout = 35 * time.Second
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("original native GET transport failed")
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
	media, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != 200 || readErr != nil || len(body) > 65536 || mediaErr != nil || media != "application/json" ||
		response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" {
		t.Fatalf("native readonly contract rejected: HTTP %d", response.StatusCode)
	}
	var fact dsgaInitializationResponse
	if dsgapi.UnmarshalObject(body, &fact) != nil || fact.validateStartup(createPayload{TenantID: input.TenantID, TimelineID: input.TimelineID}, input.OperationUUID, wanted) != nil {
		t.Fatal("native original identity or initialization proof mismatch")
	}
	// 只记录无凭据的最小事实；不输出原始配置、JWT、Seed或完整HTTP响应。
	public, _ := json.Marshal(map[string]any{
		"scope":  "Mac SSH118 original PG native initialization read-only; not project-creation E2E",
		"result": "PASS", "vm_uid": input.VMUID, "service_uid": input.ServiceUID, "config_uid": input.ConfigUID,
		"native_protocol": fact.ProtocolVersion, "bootstrap_protocol": wanted.ProtocolVersion,
		"low_buffers": fact.Observed.LowBuffers, "maximum_buffers": fact.Observed.MaximumBuffers,
		"authentication_policy_verified": fact.AuthenticationPolicyVerified, "ordinary_roles_verified": fact.OrdinaryRolesVerified,
		"mutations": false,
	})
	t.Log(string(public))
}
