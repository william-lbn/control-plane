package control

import (
	"errors"
	"os"
)

var errDSGARuntimeUnavailable = errors.New("requested PostgreSQL runtime is not configured")

// 这里只列已实现共同原生合同的适配版本；不是对现场镜像或E2E的认证。
// 具体版本是否可创建由可信发行目录、创建开关及Worker能力共同决定。
func dsgaSupportedPostgresVersion(version uint32) bool {
	return version == 17 || version == 18
}

func (policy dsgaCreationPolicy) postgresVersion() uint32 {
	if policy.PostgresVersion == 0 {
		return 17
	}
	return policy.PostgresVersion
}

// 发行目录只供平台进程配置。API请求仅选择版本/地域，不能传镜像、预算、
// 节点名单或原生协议。完整选中项随后冻结到原Operation，恢复不重读目录。
// 单版本旧变量保持兼容；同时设置两种来源必须报错，不能悄悄覆盖。
type dsgaRuntimeCatalog struct {
	ProtocolVersion uint32              `json:"protocol_version"`
	Releases        []dsgaProjectPolicy `json:"releases"`
}

func readDSGARuntimePolicies(namespace string) ([]dsgaProjectPolicy, error) {
	raw := os.Getenv("NEON_DSGA_RUNTIME_CATALOG")
	if raw == "" {
		policy, err := readDSGAProjectPolicy(namespace)
		if err != nil {
			return nil, err
		}
		return []dsgaProjectPolicy{policy}, nil
	}
	var catalog dsgaRuntimeCatalog
	if os.Getenv("NEON_DSGA_CREATION_POLICY") != "" || decodeDSGAObject([]byte(raw), &catalog) != nil || (catalog.ProtocolVersion != 1 && catalog.ProtocolVersion != 2) || len(catalog.Releases) == 0 || len(catalog.Releases) > 32 {
		return nil, errDSGACreationPlan
	}
	type key struct {
		version uint32
		region  string
	}
	seen := map[key]bool{}
	for _, policy := range catalog.Releases {
		// v2目录必须自动供应；历史v1仅保留原发行读取，不能默默补运行配置。
		if catalog.ProtocolVersion == 2 && policy.Creation.VM.supply() == nil {
			return nil, errDSGACreationPlan
		}
		// 新目录显式选择PG版本及硬预算合同v3；历史v2不被迁移或自动改写。
		if policy.Creation.PostgresVersion == 0 || policy.Creation.NativeBootstrapVersion.selected() != 3 {
			return nil, errDSGACreationPlan
		}
		if _, err := validateDSGAProjectPolicy(namespace, policy); err != nil {
			return nil, err
		}
		identity := key{policy.Creation.PostgresVersion, policy.Region}
		if seen[identity] {
			return nil, errDSGACreationPlan
		}
		seen[identity] = true
	}
	return catalog.Releases, nil
}

func selectDSGAProjectPolicy(namespace string, version uint32, region string) (dsgaProjectPolicy, error) {
	policies, err := readDSGARuntimePolicies(namespace)
	if err != nil {
		return dsgaProjectPolicy{}, err
	}
	var selected dsgaProjectPolicy
	count := 0
	for _, policy := range policies {
		if policy.Creation.postgresVersion() == version && (region == "" || policy.Region == region) {
			selected, count = policy, count+1
		}
	}
	// 未指定地域且同版本有多地域时不任意选第一项；调用者应显式选地域。
	if count != 1 {
		return dsgaProjectPolicy{}, errDSGARuntimeUnavailable
	}
	return selected, nil
}

// 公开投影仅包含产品选择项，绝不返回镜像、Service UID、节点或私有凭据来源。
// create_enabled表示平台开关，不表示创建完成；实际就绪仍由Operation及SQL回读决定。
type postgresRuntimeOption struct {
	PostgresVersion uint32       `json:"postgres_version"`
	RegionID        string       `json:"region_id"`
	CreateEnabled   bool         `json:"create_enabled"`
	MemoryScaling   string       `json:"memory_scaling"`
	Bounds          createBounds `json:"bounds"`
	MemorySlotMiB   uint64       `json:"memory_slot_mib"`
}

func (s *server) postgresRuntimeOptions() []postgresRuntimeOption {
	// 已配置内存发行目录的部署只公布该目录；不额外显示未配置的PG16。
	// 全无新目录时保留旧部署兼容入口，不将其纳入本轮PG17/18验收。
	if os.Getenv("NEON_DSGA_RUNTIME_CATALOG") == "" && os.Getenv("NEON_DSGA_CREATION_POLICY") == "" {
		return []postgresRuntimeOption{{PostgresVersion: 16, RegionID: "rke2-lab", CreateEnabled: creationEnabled(), MemoryScaling: "vm", Bounds: defaults(nil), MemorySlotMiB: 1024}}
	}
	options := []postgresRuntimeOption{}
	if s.kube == nil {
		return options
	}
	policies, err := readDSGARuntimePolicies(s.kube.namespace)
	if err != nil {
		return options
	}
	enabled := creationEnabled() && os.Getenv("NEON_DSGA_CREATE_ENABLED") == "true" && len(s.idempotencyKey) >= 32
	for _, p := range policies {
		options = append(options, postgresRuntimeOption{PostgresVersion: p.Creation.postgresVersion(), RegionID: p.Region, CreateEnabled: enabled, MemoryScaling: "vm_and_postgres", Bounds: p.Bounds, MemorySlotMiB: p.Creation.MemorySlotBytes / (1 << 20)})
	}
	return options
}
