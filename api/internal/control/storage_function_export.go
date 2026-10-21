package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functions"
	"github.com/william-lbn/control-plane/api/internal/triggers"
)

var storageEventIDPattern = regexp.MustCompile(`^soe_[a-f0-9]{32}$`)
var storageEventFunctionPattern = regexp.MustCompile(`^fnc_[a-f0-9]{16}$`)
var storageEventDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (s *server) validateStorageEvent(raw []byte, mac string, p objectStoragePayload, timeline, id string) (storageEventEnvelope, error) {
	var event storageEventEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 64<<10 || decoder.Decode(&event) != nil {
		return event, errors.New("invalid event envelope")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return event, errors.New("invalid event envelope")
	}
	if event.ID != id || !storageEventIDPattern.MatchString(id) || event.Project != p.ProjectID || event.Branch != p.BranchID || event.Timeline != timeline || event.KeyVersion != s.functions.input.KeyVersion || event.Created.IsZero() || len(s.functions.key) != 32 || len(event.Targets) < 1 || len(event.Targets) > 16 {
		return event, errors.New("invalid event identity")
	}
	if !storageBucketName.MatchString(event.Data.Bucket) || !validStorageKey(event.Data.Key) || !storageEventDigestPattern.MatchString(event.Data.SHA256) || event.Data.Size < 0 || event.Data.Size > storageObjectLimit {
		return event, errors.New("invalid event object")
	}
	seen := map[string]bool{}
	for _, t := range event.Targets {
		if !functionTriggerID.MatchString(t.Trigger) || !storageEventFunctionPattern.MatchString(t.Function) || t.Actor == "" || len(t.Actor) > 128 || t.Generation < 1 || !triggers.ValidName(t.Name) || !triggers.ValidPath(t.Path) || !functions.ValidSlug(t.Slug) || seen[t.Trigger] {
			return event, errors.New("invalid event target")
		}
		seen[t.Trigger] = true
	}
	supplied, e := hex.DecodeString(mac)
	expected, _ := hex.DecodeString(storageEventMAC(s.functions.key, event))
	if e != nil || !hmac.Equal(supplied, expected) {
		return event, errors.New("event signature rejected")
	}
	return event, nil
}

type storageEventBranch struct {
	Payload    objectStoragePayload
	Timeline   string
	Generation int64
}

func (s *server) runStorageFunctionEvents(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rows, err := s.db.Query(ctx, `SELECT m.project_id,m.branch_id,m.endpoint_id,m.database_name,m.timeline_id,m.generation FROM storage_event_branches m JOIN branches b ON b.id=m.branch_id JOIN projects p ON p.id=m.project_id JOIN endpoints e ON e.id=m.endpoint_id WHERE m.dirty AND b.project_id=m.project_id AND b.state='ready' AND b.deleted_at IS NULL AND b.timeline_id=m.timeline_id AND p.state='ready' AND p.deleted_at IS NULL AND e.branch_id=m.branch_id AND e.state IN ('active','suspended') AND e.deleted_at IS NULL ORDER BY m.updated_at,m.branch_id LIMIT 4`)
			if err != nil {
				continue
			}
			branches := []storageEventBranch{}
			for rows.Next() {
				var b storageEventBranch
				if err = rows.Scan(&b.Payload.ProjectID, &b.Payload.BranchID, &b.Payload.EndpointID, &b.Payload.Spec.Database, &b.Timeline, &b.Generation); err != nil {
					break
				}
				branches = append(branches, b)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err != nil {
				continue
			}
			for _, b := range branches {
				call, done := context.WithTimeout(ctx, 60*time.Second)
				err = s.exportStorageFunctionEvents(call, b)
				done()
				if err != nil {
					s.db.Exec(ctx, `UPDATE storage_event_branches SET error_code='event_export_unavailable',updated_at=clock_timestamp() WHERE branch_id=$1 AND generation=$2`, b.Payload.BranchID, b.Generation)
				}
			}
		}
	}
}

// Only branches marked by a publisher are opened. No cold polling of clean
// catalogs keeps SQL alive. Both publishers and this exporter hold the original
// directory lock; queue materialization is idempotent across the two databases.
func (s *server) exportStorageFunctionEvents(ctx context.Context, b storageEventBranch) error {
	p := b.Payload
	release, err := s.acquireProbeGate(ctx, p.EndpointID, true)
	if err != nil {
		return err
	}
	defer release()
	conn, err := s.dataAPIConnection(ctx, dataAPIPayload{ProjectID: p.ProjectID, EndpointID: p.EndpointID, Spec: DataAPISpec{Database: p.Spec.Database}})
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return s.exportStorageFunctionEventsConn(ctx, conn, b)
}
func (s *server) exportStorageFunctionEventsConn(ctx context.Context, conn *pgx.Conn, b storageEventBranch) error {
	p := b.Payload
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = storageDirectoryLock(ctx, tx); err != nil {
		return err
	}
	// An older active Storage database may not yet have an outbox. A first
	// publisher can commit its dirty marker then roll back native publication,
	// including lazy schema creation. Recreate the additive table under the
	// same verified directory lock so that the empty marker can be cleared.
	var marker string
	if err = tx.QueryRow(ctx, `SELECT COALESCE(obj_description(oid,'pg_namespace'),'') FROM pg_namespace WHERE nspname='neon_storage'`).Scan(&marker); err != nil || marker != "neon-control:object-storage:"+p.ProjectID {
		return errors.New("storage event schema ownership mismatch")
	}
	if _, err = tx.Exec(ctx, objectEventsSchema, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id,envelope,mac FROM neon_storage.object_events WHERE project_id=$1 AND branch_id=$2 AND timeline_id=$3 AND state='pending' ORDER BY created_at,id LIMIT 16 FOR UPDATE`, p.ProjectID, p.BranchID, b.Timeline)
	if err != nil {
		return err
	}
	type item struct {
		id  string
		raw []byte
		mac string
	}
	items := []item{}
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.raw, &v.mac); err != nil {
			rows.Close()
			return err
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	metadata, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer metadata.Rollback(ctx)
	for _, v := range items {
		event, e := s.validateStorageEvent(v.raw, v.mac, p, b.Timeline, v.id)
		if e != nil {
			if _, err = tx.Exec(ctx, `UPDATE neon_storage.object_events SET state='rejected',error_code='event_authentication_rejected',exported_at=clock_timestamp() WHERE id=$1`, v.id); err != nil {
				return err
			}
			continue
		}
		for _, target := range event.Targets {
			id := storageEventInvocationID(event.Project, event.Branch, event.ID, target.Trigger)
			snapshot := functionTriggerSnapshot{ID: target.Trigger, Name: target.Name, Slug: target.Slug, Path: target.Path, Type: "storage_object_created", LogicalID: id, Object: &event.Data}
			body, e := json.Marshal(snapshot)
			if e != nil {
				return e
			}
			var exists bool
			if err = metadata.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM function_trigger_invocations WHERE id=$1)`, id).Scan(&exists); err != nil {
				return err
			}
			if exists {
				var same bool
				err = metadata.QueryRow(ctx, `SELECT project_id=$2 AND branch_id=$3 AND trigger_id=$4 AND function_id=$5 AND actor_id=$6 AND trigger_generation=$7 AND object_event_id=$8 AND snapshot=$9::jsonb AND parent_invocation_id IS NULL FROM function_trigger_invocations WHERE id=$1`, id, event.Project, event.Branch, target.Trigger, target.Function, target.Actor, target.Generation, event.ID, body).Scan(&same)
				if err != nil || !same {
					return errors.New("existing event occurrence identity mismatch")
				}
				continue
			}
			var queued int
			if err = metadata.QueryRow(ctx, `SELECT count(*) FROM function_trigger_invocations WHERE branch_id=$1 AND state IN ('queued','dispatching')`, event.Branch).Scan(&queued); err != nil {
				return err
			}
			if queued >= 512 {
				return errors.New("function object event queue full")
			}
			_, err = metadata.Exec(ctx, `INSERT INTO function_trigger_invocations(id,trigger_id,project_id,branch_id,function_id,actor_id,trigger_generation,scheduled_at,snapshot,state,object_event_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'queued',$10)`, id, target.Trigger, event.Project, event.Branch, target.Function, target.Actor, target.Generation, event.Created, body, event.ID)
			if err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE neon_storage.object_events SET state='exported',error_code=NULL,exported_at=clock_timestamp() WHERE id=$1`, v.id); err != nil {
			return err
		}
	}
	// Queue first, native acknowledgment second, marker CAS last. A crash at any
	// boundary retains the original occurrence and cannot erase a newer publisher.
	if err = metadata.Commit(ctx); err != nil {
		return err
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM neon_storage.object_events WHERE project_id=$1 AND branch_id=$2 AND timeline_id=$3 AND state='pending')`, p.ProjectID, p.BranchID, b.Timeline).Scan(&pending); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `UPDATE storage_event_branches SET dirty=$3,error_code=NULL,updated_at=clock_timestamp() WHERE branch_id=$1 AND generation=$2`, p.BranchID, b.Generation, pending)
	return err
}
