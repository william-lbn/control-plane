package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/william-lbn/control-plane/api/internal/realtime"
)

const registerRealtimeSource = "register_realtime_source"
const retireRealtimeSource = "retire_realtime_source"

var realtimeDatabaseName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
var realtimeIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

type realtimeSourceSpec struct {
	Database    string                 `json:"database"`
	Publication string                 `json:"publication"`
	Relations   []dsgaRealtimeRelation `json:"relations"`
}

func (p realtimeSourceSpec) valid() bool {
	if !realtimeDatabaseName.MatchString(p.Database) || !realtimeDatabaseName.MatchString(p.Publication) ||
		len(p.Relations) == 0 || len(p.Relations) > 32 {
		return false
	}
	seen := make(map[dsgaRealtimeRelation]bool, len(p.Relations))
	for _, r := range p.Relations {
		schema := strings.ToLower(r.Schema)
		if !realtimeIdentifier.MatchString(r.Schema) || !realtimeIdentifier.MatchString(r.Name) ||
			strings.HasPrefix(schema, "pg_") || strings.HasPrefix(schema, "neon_") ||
			strings.HasPrefix(schema, "control_") || schema == "information_schema" || seen[r] {
			return false
		}
		seen[r] = true
	}
	return true
}

type realtimeSourceOperation struct {
	ID         string             `json:"id"`
	ProjectID  string             `json:"project_id"`
	BranchID   string             `json:"branch_id"`
	WriterID   string             `json:"writer_id"`
	TimelineID string             `json:"timeline_id"`
	MaxWAL     int64              `json:"max_wal_bytes"`
	Spec       realtimeSourceSpec `json:"spec"`
}

func (p realtimeSourceOperation) valid() bool {
	return strings.HasPrefix(p.ID, "rts_") && dsgaRealtimeLoginID.MatchString(p.ID) &&
		p.ProjectID != "" && dataBranchID.MatchString(p.BranchID) && p.WriterID != "" &&
		dsgaStorageID.MatchString(p.TimelineID) && p.MaxWAL > 0 && p.Spec.valid()
}

func (p realtimeSourceOperation) sourceKey() realtime.SourceKey {
	return realtime.SourceKey{ID: p.ID, Scope: realtime.Scope{
		Project: p.ProjectID, Branch: p.BranchID, Timeline: p.TimelineID, Database: p.Spec.Database,
	}}
}

func (p realtimeSourceOperation) relations() []realtime.Relation {
	out := make([]realtime.Relation, 0, len(p.Spec.Relations))
	for _, r := range p.Spec.Relations {
		out = append(out, realtime.Relation{Schema: r.Schema, Name: r.Name})
	}
	return out
}

var realtimeSourceRegistrationSteps = []string{"reserve_realtime_credential", "publish_realtime_login",
	"confirm_realtime_slot", "commit_realtime_source"}
var realtimeSourceRetirementSteps = []string{"stop_realtime_demand", "release_realtime_slot",
	"revoke_realtime_login", "retire_realtime_credential"}

func (s *server) acceptRealtimeSource(w http.ResponseWriter, r *http.Request, id string) {
	project := r.PathValue("project")
	operation, err := s.operationRecord(r.Context(), id, project)
	if err != nil || operation["resource_type"] != "realtime_source" {
		fail(w, r, 503, "metadata_unavailable", "Could not load Realtime Operation")
		return
	}
	w.Header().Set("Location", "/api/v1/projects/"+project+"/operations/"+id)
	jsonResponse(w, http.StatusAccepted, map[string]any{"source_id": operation["resource_id"], "operation": operation})
}

func (s *server) replayRealtimeSource(w http.ResponseWriter, r *http.Request, db connectionDefaultReader, key, hash string) bool {
	var storedHash, operation string
	err := db.QueryRow(r.Context(), "SELECT request_hash,operation_id FROM idempotency_keys WHERE actor_id=$1 AND key_hash=$2",
		userFrom(r).ID, keyHash(key)).Scan(&storedHash, &operation)
	if isNoRows(err) {
		return false
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check Realtime idempotency")
	} else if storedHash != hash {
		fail(w, r, 409, "idempotency_conflict", "Key used with different input")
	} else {
		s.acceptRealtimeSource(w, r, operation)
	}
	return true
}

func (s *server) realtimeSources(w http.ResponseWriter, r *http.Request) {
	project, branch := r.PathValue("project"), r.PathValue("branch")
	items, err := s.many(r.Context(), `SELECT p.id,p.database_name,p.writer_id,p.state,p.operation_id,
 (o.payload->'spec'->>'publication') AS publication
 FROM realtime_source_operations p JOIN operations o ON o.id=p.operation_id
 JOIN branches b ON b.id=p.branch_id AND b.project_id=p.project_id AND b.deleted_at IS NULL
 WHERE p.project_id=$1 AND p.branch_id=$2 AND p.state<>'retired' ORDER BY p.id LIMIT 100`, project, branch)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not load Realtime sources")
		return
	}
	jsonResponse(w, http.StatusOK, record{"items": items, "enabled": s.realtime != nil})
}

func (s *server) mutateRealtimeSource(w http.ResponseWriter, r *http.Request) {
	if accessFrom(r).Level < 3 {
		fail(w, r, 403, "forbidden", "Realtime source management requires project administration")
		return
	}
	register := r.Method == http.MethodPost
	if len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "realtime_unavailable", "Realtime source management is unavailable")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	version, ok := lifecycleVersion(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	p := realtimeSourceOperation{ProjectID: r.PathValue("project"), BranchID: r.PathValue("branch"), ID: r.PathValue("rtSource")}
	action, steps := retireRealtimeSource, realtimeSourceRetirementSteps
	if register {
		action, steps = registerRealtimeSource, realtimeSourceRegistrationSteps
		if readJSON(r, &p.Spec) != nil || !p.Spec.valid() {
			fail(w, r, 422, "invalid_realtime_source", "Choose an explicit publication and its registered tables")
			return
		}
		sort.Slice(p.Spec.Relations, func(i, j int) bool {
			a, b := p.Spec.Relations[i], p.Spec.Relations[j]
			return a.Schema < b.Schema || a.Schema == b.Schema && a.Name < b.Name
		})
	}
	hash := s.createRequestHash(action+":"+p.ProjectID+":"+p.BranchID, record{"source": p.ID, "spec": p.Spec, "writer_version": version})
	if s.replayRealtimeSource(w, r, s.db, key, hash) {
		return
	}
	if (register && s.realtime == nil) || s.kube == nil {
		fail(w, r, 503, "realtime_unavailable", "Realtime source management is unavailable")
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not admit Realtime Operation")
		return
	}
	defer tx.Rollback(ctx)
	var actualVersion int64
	err = tx.QueryRow(ctx, `SELECT e.id,b.timeline_id,e.version FROM branches b
 JOIN endpoints e ON e.branch_id=b.id AND e.project_id=b.project_id
 WHERE b.id=$1 AND b.project_id=$2 AND b.state='ready' AND b.deleted_at IS NULL
 AND e.endpoint_type='read_write' AND e.state='active' AND e.deleted_at IS NULL FOR UPDATE OF b,e`,
		p.BranchID, p.ProjectID).Scan(&p.WriterID, &p.TimelineID, &actualVersion)
	if err != nil {
		fail(w, r, 409, "writer_required", "The original active Writer is required")
		return
	}
	if s.replayRealtimeSource(w, r, tx, key, hash) {
		return
	}
	if actualVersion != version {
		fail(w, r, 412, "version_conflict", "Writer version changed")
		return
	}
	if register {
		runtime, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, p.WriterID)
		if err != nil || runtime == nil || runtime.Payload.BranchID != p.BranchID || runtime.Payload.TimelineID != p.TimelineID ||
			runtime.Plan.SlotWALKeepMiB == 0 {
			fail(w, r, 409, "realtime_wal_budget_required", "Writer must have a finite frozen slot retention budget")
			return
		}
		p.ID, p.MaxWAL = newID("rts_"), int64(runtime.Plan.SlotWALKeepMiB)<<20
	} else {
		var raw []byte
		err = tx.QueryRow(ctx, `SELECT payload FROM realtime_source_operations r JOIN operations o ON o.id=r.operation_id
 WHERE r.id=$1 AND r.project_id=$2 AND r.branch_id=$3 AND r.timeline_id=$4 AND r.writer_id=$5 AND r.state IN ('registering','ready')
 AND o.state IN ('succeeded','failed','cancelled') FOR UPDATE OF r`,
			p.ID, p.ProjectID, p.BranchID, p.TimelineID, p.WriterID).Scan(&raw)
		if err != nil || json.Unmarshal(raw, &p) != nil || !p.valid() {
			fail(w, r, 409, "realtime_source_unavailable", "Original Realtime source is unavailable")
			return
		}
		var held bool
		err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT FROM realtime_queries WHERE source_id=$1 AND retired_at IS NULL)", p.ID).Scan(&held)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not check Realtime queries")
			return
		}
		if held {
			fail(w, r, 409, "realtime_queries_active", "Retire the queries before releasing their source")
			return
		}
	}
	op := newID("op_")
	raw, _ := json.Marshal(p)
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
 VALUES($1,$2,'realtime_source',$3,$4,'queued',$5,$6,$7)`,
		op, p.ProjectID, p.ID, action, userFrom(r).ID, requestID(r), raw)
	if register && err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO realtime_source_operations(id,project_id,branch_id,timeline_id,writer_id,database_name,operation_id,state)
 VALUES($1,$2,$3,$4,$5,$6,$7,'registering')`, p.ID, p.ProjectID, p.BranchID, p.TimelineID, p.WriterID, p.Spec.Database, op)
	} else if err == nil {
		_, err = tx.Exec(ctx, "UPDATE realtime_source_operations SET operation_id=$2,state='retiring' WHERE id=$1", p.ID, op)
	}
	for i, step := range steps {
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, op, i, step)
		}
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`,
			userFrom(r).ID, keyHash(key), hash, op)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			fail(w, r, 409, "realtime_source_conflict", "A source already owns this database")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not admit Realtime Operation")
		}
		return
	}
	s.acceptRealtimeSource(w, r, op)
}
