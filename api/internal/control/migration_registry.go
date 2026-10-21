package control

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const dsga058JoinMigration = "900000059_dsga_creation_and_catalog_058_join.sql"

// 旧元数据库只按integer登记迁移，两个文件同号会静默跳过后者。执行任何DDL前
// 审核整个registry，默认拒绝重复；唯一例外是已经分别发布的两份058，并且必须
// 同时包含前向join。原文件保持字节不变，join对两种历史来源补齐各自缺失DDL。
// 这不是允许任意冲突的通用逃生口；后续DSGA使用独立高位保留段，不重命名历史。
func validateMigrationRegistry(names []string) error {
	versions := make(map[int][]string, len(names))
	join := false
	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil || version <= 0 || uint64(version) > uint64(1<<31-1) {
			return fmt.Errorf("invalid integer migration version: %s", name)
		}
		versions[version] = append(versions[version], name)
		if name == dsga058JoinMigration {
			join = true
		}
	}
	for version, files := range versions {
		if len(files) < 2 {
			continue
		}
		sort.Strings(files)
		if version == 58 && len(files) == 2 && files[0] == "058_catalog_current_observations.sql" && files[1] == "058_dsga_creation_identity.sql" && join {
			continue
		}
		return fmt.Errorf("duplicate migration version %d has no reviewed forward join: %v", version, files)
	}
	return nil
}
