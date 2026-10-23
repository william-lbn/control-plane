package control

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

const dsgaApplicationRole = "app_owner"
const dsgaApplicationDatabase = "neondb"

var errDSGARender = errors.New("DSGA compute rendering requires the original safe creation inputs")

// Renderer只接受已经持久化的业务verifier和管理公钥，不接收明文密码、管理PG
// 口令或签名seed。调用方须先核对原Operation、VM UID、凭据Secret UID/内容，
// RO须使用源timeline的同一verifier；本纯函数不读取Secret、不补盐、不轮转密钥。
// 这不是worker已接线的声明，也不提供预算/来源/物理完成许可。
type dsgaRenderCredentials struct {
	ApplicationVerifier string
	ProbeVerifier       string
	ControlPublicKey    ed25519.PublicKey
}

// typed JSON避免预算经map[string]any/float64往返，并限制可输出的配置面。
// 不导入旧实验资产：其中fsync=off、md5及cloud_admin租户密码均不适用DSGA。
// 运行时catalog投影必须另有冻结来源；不能将CatalogSpec字符串无校验拼进启动。
type dsgaRenderOption struct {
	Name    string  `json:"name"`
	Value   *string `json:"value"`
	VarType string  `json:"vartype"`
}

type dsgaRenderRole struct {
	Name     string             `json:"name"`
	Verifier string             `json:"encrypted_password"`
	Options  []dsgaRenderOption `json:"options"`
}

// renderDSGAComputeBootstrap固定原UUID/时间/原生预算和实际VM UID，重复输入输出
// 逐字节相同；无env、时钟、随机数、网络或本地文件副作用。先完整校验再返回
// 唯一文档，错误不返回部分配置。管理TLS材料由独立已绑定挂载提供，仅输出路径。
// 当前是安全bootstrap Renderer；完整catalog投影/公开worker/来源回执仍须接线，
// 不允许调用方因成功序列化就将普通SQL准入打开或宣称VM shrink已完成。
func renderDSGAComputeBootstrap(p createPayload, plan dsgaCreationPlan, vmUID string, credentials dsgaRenderCredentials) ([]byte, error) {
	native, err := plan.nativeBootstrap(p, vmUID)
	if err != nil || p.CatalogSpec != "" || p.CatalogPolicy != "" || p.CatalogInitializePlatform ||
		!functionsql.ValidVerifier(credentials.ApplicationVerifier) || !functionsql.ValidVerifier(credentials.ProbeVerifier) ||
		len(credentials.ControlPublicKey) != ed25519.PublicKeySize || bytes.Equal(credentials.ControlPublicKey, make([]byte, ed25519.PublicKeySize)) {
		return nil, errDSGARender
	}
	option := func(name, value, kind string) dsgaRenderOption {
		return dsgaRenderOption{name, &value, kind}
	}
	flags := func(createdb bool, connections string) []dsgaRenderOption {
		create := "NOCREATEDB"
		if createdb {
			create = "CREATEDB"
		}
		var result []dsgaRenderOption
		// 显式给负权限，避免原生角色创建默认值补入CREATEROLE/REPLICATION/
		// BYPASSRLS。NOSUPERUSER之外仍由原生权限图校验弱组及既有成员关系。
		for _, flag := range []string{"NOSUPERUSER", "LOGIN", "INHERIT", create, "NOCREATEROLE", "NOREPLICATION", "NOBYPASSRLS"} {
			result = append(result, dsgaRenderOption{Name: flag, VarType: "enum"})
		}
		return append(result, option("CONNECTION LIMIT", connections, "integer"))
	}
	roles := []dsgaRenderRole{
		{dsgaApplicationRole, credentials.ApplicationVerifier, flags(true, "100")},
		{"control_probe", credentials.ProbeVerifier, flags(false, "4")},
	}
	settings := []dsgaRenderOption{
		option("fsync", "on", "bool"),
		option("wal_level", "logical", "enum"),
		option("wal_log_hints", "on", "bool"),
		option("port", "55433", "integer"),
		option("listen_addresses", "0.0.0.0", "string"),
		option("password_encryption", "scram-sha-256", "enum"),
		option("restart_after_crash", "off", "bool"),
		option("max_connections", "100", "integer"),
		option("superuser_reserved_connections", "5", "integer"),
		option("max_wal_senders", "10", "integer"),
		option("max_replication_slots", "10", "integer"),
		option("wal_sender_timeout", "5s", "string"),
		option("shared_preload_libraries", "neon,pg_stat_statements", "string"),
		// 未登记预算/排空接口的后台扩展不能沿用旧模板自动预载；LFC同样不
		// 从旧规格推测占用。完整预算worker到位后才允许按受支持profile启用。
		option("neon.max_file_cache_size", "0", "string"),
	}
	mode := "Primary"
	safekeepers := []string{"safekeeper1:5454", "safekeeper2:5454", "safekeeper3:5454"}
	if p.EndpointType == "read_only" {
		mode = "Replica"
		settings = append(settings,
			option("primary_conninfo", "host=safekeeper1,safekeeper2,safekeeper3 port=5454,5454,5454 options='-c timeline_id="+p.TimelineID+" tenant_id="+p.TenantID+"' application_name=replica replication=true", "string"),
			option("primary_slot_name", "repl_"+p.TimelineID+"_", "string"),
			option("recovery_prefetch", "off", "enum"))
	} else {
		settings = append(settings, option("synchronous_standby_names", "walproposer", "string"))
	}
	public := credentials.ControlPublicKey
	id := sha256.Sum256(public)
	// 所有map均为固定字段，不承载外部任意值或数字预算；encoding/json稳定排序。
	// 业务verifier只进入普通角色。唯一管理认证信息是公钥；不能复用角色列表
	// 为cloud_admin/postgres/zenith_admin补任何口令，也不启用环回JWT trust名单。
	document := map[string]any{
		"spec": map[string]any{
			"format_version": 1, "timestamp": plan.AcceptedAt, "operation_uuid": plan.OperationUUID,
			"dynamic_shared_buffers": native, "mode": mode, "suspend_timeout_seconds": -1,
			"project_id": p.ProjectID, "branch_id": p.BranchID, "endpoint_id": p.EndpointID,
			"tenant_id": p.TenantID, "timeline_id": p.TimelineID,
			"pageserver_connstring": "host=pageserver-managed port=6400", "safekeeper_connstrings": safekeepers,
			"skip_pg_catalog_updates": false, "disable_lfc_resizing": true,
			"cluster": map[string]any{
				"cluster_id": p.ProjectID, "name": p.ProjectID, "state": "restarted", "roles": roles,
				"databases": []any{map[string]any{"name": dsgaApplicationDatabase, "owner": dsgaApplicationRole, "options": nil}},
				"settings":  settings,
			},
			"delta_operations": []any{},
		},
		"compute_ctl_config": map[string]any{
			"jwks": map[string]any{"keys": []any{map[string]any{"use": "sig", "key_ops": []string{"verify"}, "alg": "EdDSA", "kid": hex.EncodeToString(id[:16]), "kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(public)}}},
			"tls":  map[string]string{"key_path": "/run/neon/management/control.key", "cert_path": "/run/neon/management/control.crt"},
		},
	}
	encoded, err := json.Marshal(document)
	if err != nil || len(encoded) > 1<<20 {
		return nil, errDSGARender
	}
	return encoded, nil
}
