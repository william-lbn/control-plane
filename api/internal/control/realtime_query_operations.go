package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/william-lbn/control-plane/api/internal/realtime"
)

const registerRealtimeQuery = "register_realtime_query"
const retireRealtimeQuery = "retire_realtime_query"
const realtimeQueryOwner = "neon-control/realtime-query"

var realtimeSelect = regexp.MustCompile(`(?i)^(select|with)\b`)

type realtimeQuerySpec struct {
	Name           string                 `json:"name"`
	Statement      string                 `json:"statement"`
	Relations      []dsgaRealtimeRelation `json:"relations"`
	ParameterCount int                    `json:"parameter_count"`
	MaxRows        int                    `json:"max_rows"`
	MaxResultBytes int                    `json:"max_result_bytes"`
	QueryTimeoutMS int                    `json:"query_timeout_ms"`
}

func (q realtimeQuerySpec) valid() bool {
	return realtimeQueryName.MatchString(q.Name) && len(q.Statement) <= 65536 &&
		realtimeSelect.MatchString(q.Statement) && validRealtimeRelations(q.Relations) &&
		q.ParameterCount >= 0 && q.ParameterCount <= 32 && q.MaxRows > 0 && q.MaxRows <= 2000 &&
		q.MaxResultBytes > 0 && q.MaxResultBytes <= 1<<20 && q.QueryTimeoutMS > 0 && q.QueryTimeoutMS <= 30000
}

type realtimeQueryOperation struct {
	ID                  string            `json:"id"`
	SourceID            string            `json:"source_id"`
	ProjectID           string            `json:"project_id"`
	BranchID            string            `json:"branch_id"`
	WriterID            string            `json:"writer_id"`
	TimelineID          string            `json:"timeline_id"`
	Database            string            `json:"database"`
	CredentialNamespace string            `json:"credential_namespace"`
	Spec                realtimeQuerySpec `json:"spec"`
}

func (q realtimeQueryOperation) valid() bool {
	return strings.HasPrefix(q.ID, "rtq_") && dsgaRealtimeLoginID.MatchString(q.ID) &&
		strings.HasPrefix(q.SourceID, "rts_") && dsgaRealtimeLoginID.MatchString(q.SourceID) &&
		q.ProjectID != "" && q.WriterID != "" && dataBranchID.MatchString(q.BranchID) &&
		dsgaStorageID.MatchString(q.TimelineID) && realtimeDatabaseName.MatchString(q.Database) &&
		len(q.CredentialNamespace) <= 63 && dsgaPlanNamespace.MatchString(q.CredentialNamespace) && q.Spec.valid()
}

func (q realtimeQueryOperation) loginBinding(uid string) realtimeLoginBinding {
	return realtimeLoginBinding{key: realtime.SourceKey{ID: q.SourceID, Scope: realtime.Scope{
		Project: q.ProjectID, Branch: q.BranchID, Timeline: q.TimelineID, Database: q.Database,
	}}, writer: q.WriterID, role: realtimeRole(q.ID), credential: "neon-rt-" + q.ID[4:],
		credentialNamespace: q.CredentialNamespace, credentialUID: uid, ownerLabel: realtimeQueryOwner, ownerID: q.ID}
}

var realtimeQueryRegistrationSteps = []string{"reserve_realtime_query_credential", "publish_realtime_query_login", "commit_realtime_query"}
var realtimeQueryRetirementSteps = []string{"retire_realtime_query_route", "revoke_realtime_query_login", "retire_realtime_query_credential"}

func (s *server) realtimeQueries(w http.ResponseWriter, r *http.Request) {
	items, err := s.many(r.Context(), `SELECT q.id,q.query_name,q.state,q.operation_id,q.writer_id
 FROM realtime_query_operations q JOIN realtime_sources s ON s.id=q.source_id
 JOIN branches b ON b.id=q.branch_id AND b.project_id=q.project_id AND b.deleted_at IS NULL
 WHERE q.project_id=$1 AND q.branch_id=$2 AND q.source_id=$3 AND q.state<>'retired' AND s.retired_at IS NULL
 ORDER BY q.id LIMIT 100`, r.PathValue("project"), r.PathValue("branch"), r.PathValue("rtSource"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not load Realtime queries")
		return
	}
	jsonResponse(w, http.StatusOK, record{"items": items, "enabled": s.realtime != nil})
}

func (s *server) mutateRealtimeQuery(w http.ResponseWriter, r *http.Request) {
	if accessFrom(r).Level < 3 {
		fail(w, r, 403, "forbidden", "Realtime query management requires project administration")
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
	p := realtimeQueryOperation{ProjectID: r.PathValue("project"), BranchID: r.PathValue("branch"),
		SourceID: r.PathValue("rtSource"), ID: r.PathValue("rtQueryID")}
	register := r.Method == http.MethodPost
	action, steps := retireRealtimeQuery, realtimeQueryRetirementSteps
	if register {
		action, steps = registerRealtimeQuery, realtimeQueryRegistrationSteps
		p.Spec.MaxRows, p.Spec.MaxResultBytes, p.Spec.QueryTimeoutMS = 200, 256<<10, 5000
		err := readJSON(r, &p.Spec)
		p.Spec.Statement = strings.TrimSpace(p.Spec.Statement)
		if err != nil || !p.Spec.valid() {
			fail(w, r, 422, "invalid_realtime_query", "Provide a bounded named SELECT and its source relations")
			return
		}
		sortRealtimeRelations(p.Spec.Relations)
	}
	if len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "realtime_unavailable", "Realtime query management is unavailable")
		return
	}
	hash := s.createRequestHash(action+":"+p.ProjectID+":"+p.BranchID,
		record{"source": p.SourceID, "query": p.ID, "spec": p.Spec, "writer_version": version})
	if s.replayRealtimeOperation(w, r, s.db, key, hash, "query") {
		return
	}
	if (register && s.realtime == nil) || s.kube == nil {
		fail(w, r, 503, "realtime_unavailable", "Realtime query management is unavailable")
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not admit Realtime query Operation")
		return
	}
	defer tx.Rollback(ctx)
	var actualVersion int64
	var permitted bool
	err = tx.QueryRow(ctx, `SELECT s.timeline_id,s.database_name,e.id,e.version,$4::jsonb <@ s.relations
 FROM realtime_sources s JOIN realtime_source_runtime a ON a.source_id=s.id
 JOIN realtime_source_operations o ON o.id=s.id AND o.state='ready'
 JOIN branches b ON b.id=s.branch_id AND b.project_id=s.project_id AND b.timeline_id=s.timeline_id
 JOIN endpoints e ON e.id=a.writer_id AND e.project_id=s.project_id AND e.branch_id=s.branch_id
 WHERE s.id=$1 AND s.project_id=$2 AND s.branch_id=$3 AND s.retired_at IS NULL
 AND b.state='ready' AND b.deleted_at IS NULL AND e.state='active' AND e.deleted_at IS NULL AND e.endpoint_type='read_write'
 FOR UPDATE OF b,e,o`, p.SourceID, p.ProjectID, p.BranchID, realtimeRelations(p.Spec.Relations)).
		Scan(&p.TimelineID, &p.Database, &p.WriterID, &actualVersion, &permitted)
	if err != nil {
		fail(w, r, 409, "realtime_source_unavailable", "A ready Realtime source and its active Writer are required")
		return
	}
	if s.replayRealtimeOperation(w, r, tx, key, hash, "query") {
		return
	}
	if actualVersion != version {
		fail(w, r, 412, "version_conflict", "Writer version changed")
		return
	}
	if register {
		if !permitted {
			fail(w, r, 422, "unregistered_realtime_relation", "The query relations must belong to the original source")
			return
		}
		p.CredentialNamespace = s.realtimeCredentialNamespace()
		if p.CredentialNamespace == "" {
			fail(w, r, 503, "realtime_credentials_unavailable", "Dedicated Realtime credential namespace required")
			return
		}
		p.ID = newID("rtq_")
	} else {
		var raw []byte
		err = tx.QueryRow(ctx, `SELECT o.payload FROM realtime_query_operations q JOIN operations o ON o.id=q.operation_id
 WHERE q.id=$1 AND q.source_id=$2 AND q.project_id=$3 AND q.branch_id=$4 AND q.writer_id=$5
 AND q.state IN ('registering','ready') AND o.state IN ('succeeded','failed','cancelled') FOR UPDATE OF q`,
			p.ID, p.SourceID, p.ProjectID, p.BranchID, p.WriterID).Scan(&raw)
		if err != nil || json.Unmarshal(raw, &p) != nil || !p.valid() {
			fail(w, r, 409, "realtime_query_unavailable", "Original Realtime query is unavailable")
			return
		}
	}
	op := newID("op_")
	raw, _ := json.Marshal(p)
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
 VALUES($1,$2,'realtime_query',$3,$4,'queued',$5,$6,$7)`,
		op, p.ProjectID, p.ID, action, userFrom(r).ID, requestID(r), raw)
	if register && err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO realtime_query_operations(id,source_id,project_id,branch_id,writer_id,query_name,operation_id,state)
 VALUES($1,$2,$3,$4,$5,$6,$7,'registering')`, p.ID, p.SourceID, p.ProjectID, p.BranchID, p.WriterID, p.Spec.Name, op)
	} else if err == nil {
		_, err = tx.Exec(ctx, "UPDATE realtime_query_operations SET operation_id=$2,state='retiring' WHERE id=$1", p.ID, op)
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
			fail(w, r, 409, "realtime_query_conflict", "The query name is already registered")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not admit Realtime query Operation")
		}
		return
	}
	s.acceptRealtimeOperation(w, r, op, "query")
}
