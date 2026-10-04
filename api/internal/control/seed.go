package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
)

type seedData struct {
	SchemaVersion int      `json:"schema_version"`
	Projects      []record `json:"projects"`
	Branches      []record `json:"branches"`
	Endpoints     []record `json:"endpoints"`
}

func flag(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	}
	return false
}
func integer(value any) any {
	if value == nil {
		return nil
	}
	if n, ok := value.(float64); ok {
		return int64(n)
	}
	return value
}

func (s *server) seedLab(ctx context.Context) error {
	file := os.Getenv("NEON_V2_SEED_FILE")
	if file == "" {
		return nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var data seedData
	if err := json.Unmarshal(b, &data); err != nil {
		return err
	}
	if data.SchemaVersion != 1 {
		return errors.New("unsupported lab seed version")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, p := range data.Projects {
		_, err = tx.Exec(ctx, `INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,default_branch_id,
            protected,version,created_at,updated_at,deleted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
            ON CONFLICT(id) DO NOTHING`, p["id"], p["org_id"], p["name"], p["tenant_id"], p["region_id"], integer(p["postgres_version"]),
			p["state"], p["source"], p["default_branch_id"], flag(p["protected"]), integer(p["version"]), p["created_at"], p["updated_at"], p["deleted_at"])
		if err != nil {
			return err
		}
	}
	for _, b := range data.Branches {
		_, err = tx.Exec(ctx, `INSERT INTO branches(id,project_id,name,timeline_id,parent_branch_id,parent_lsn,is_default,
            protected,state,version,created_at,deleted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
            ON CONFLICT(id) DO NOTHING`, b["id"], b["project_id"], b["name"], b["timeline_id"], b["parent_branch_id"],
			b["parent_lsn"], flag(b["is_default"]), flag(b["protected"]), b["state"], integer(b["version"]), b["created_at"], b["deleted_at"])
		if err != nil {
			return err
		}
	}
	for _, e := range data.Endpoints {
		_, err = tx.Exec(ctx, `INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,
            state,role_name,database_name,min_cpu_milli,max_cpu_milli,min_memory_mib,max_memory_mib,slot_size_mib,
            scale_to_zero,version,created_at,updated_at,deleted_at)
            VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
            ON CONFLICT(id) DO NOTHING`, e["id"], e["project_id"], e["branch_id"], e["selector"], e["workload_kind"],
			e["workload_name"], e["service_name"], e["state"], e["role_name"], e["database_name"], integer(e["min_cpu_milli"]),
			integer(e["max_cpu_milli"]), integer(e["min_memory_mib"]), integer(e["max_memory_mib"]), integer(e["slot_size_mib"]), flag(e["scale_to_zero"]),
			integer(e["version"]), e["created_at"], e["updated_at"], e["deleted_at"])
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO branch_service_instances(branch_id,service_kind,desired_state,observed_state,driver_version,last_observed_at)
        SELECT b.id,'postgres','active',CASE WHEN b.state='ready' THEN 'active' ELSE 'provisioning' END,
               'open-source-neon',now() FROM branches b
        ON CONFLICT(branch_id,service_kind) DO NOTHING`)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
