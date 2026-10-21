package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type storageCreatedData struct {
	Bucket string `json:"bucket_name"`
	Key    string `json:"object_key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type storageEventTarget struct {
	Trigger    string `json:"trigger_id"`
	Function   string `json:"function_id"`
	Actor      string `json:"actor_id"`
	Generation int64  `json:"generation"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Path       string `json:"path"`
}
type storageEventEnvelope struct {
	ID         string               `json:"id"`
	Project    string               `json:"project_id"`
	Branch     string               `json:"branch_id"`
	Timeline   string               `json:"timeline_id"`
	KeyVersion string               `json:"key_version"`
	Created    time.Time            `json:"created_at"`
	Data       storageCreatedData   `json:"data"`
	Targets    []storageEventTarget `json:"targets"`
}

func (s *server) functionObjectEventsEnabled() bool {
	return s.functions != nil && s.storage != nil && functionPublicOrigin() != "" && os.Getenv("NEON_FUNCTION_OBJECT_EVENTS_ENABLED") == "true"
}

func storageEventMAC(key []byte, event storageEventEnvelope) string {
	raw, _ := json.Marshal(event)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("neon.storage.function-event.v1\x00"))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}
func storageEventInvocationID(project, branch, event, trigger string) string {
	sum := sha256.Sum256([]byte("neon.storage.function-occurrence.v1\x00" + project + "\x00" + branch + "\x00" + event + "\x00" + trigger))
	return "fti_" + hex.EncodeToString(sum[:])
}

// Called only while holding storageDirectoryLock in the SAME transaction as
// logical object publication. An operator pause retains already enabled targets
// in the outbox; it pauses dispatch, never silently discards published events.
func (s *server) publishStorageFunctionEvent(ctx context.Context, tx pgx.Tx, p objectStoragePayload, bucket, key, digest string, size int64) error {
	if s.functions == nil || s.db == nil {
		return nil
	}
	rows, err := s.db.Query(ctx, `SELECT t.id,t.function_id,t.actor_id,t.generation,t.name,f.slug,t.function_path FROM function_triggers t JOIN function_definitions f ON f.id=t.function_id WHERE t.project_id=$1 AND t.branch_id=$2 AND t.type='storage_object_created' AND t.state='ready' AND t.enabled AND t.bucket_name=$3 AND left($4,char_length(t.key_prefix))=t.key_prefix AND f.state NOT IN ('deleting','deleted') ORDER BY t.id LIMIT 17`, p.ProjectID, p.BranchID, bucket, key)
	if err != nil {
		return err
	}
	targets := []storageEventTarget{}
	for rows.Next() {
		var t storageEventTarget
		if err = rows.Scan(&t.Trigger, &t.Function, &t.Actor, &t.Generation, &t.Name, &t.Slug, &t.Path); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	if len(targets) > 16 {
		return errors.New("storage trigger target budget exceeded")
	}
	if len(s.functions.key) != 32 || s.functions.input.KeyVersion == "" {
		return errors.New("storage event signing key unavailable")
	}
	var timeline string
	if err = s.db.QueryRow(ctx, `SELECT timeline_id FROM branches WHERE id=$1 AND project_id=$2 AND state='ready' AND deleted_at IS NULL`, p.BranchID, p.ProjectID).Scan(&timeline); err != nil {
		return err
	}
	var marker string
	if err = tx.QueryRow(ctx, `SELECT COALESCE(obj_description(oid,'pg_namespace'),'') FROM pg_namespace WHERE nspname='neon_storage'`).Scan(&marker); err != nil || marker != "neon-control:object-storage:"+p.ProjectID {
		return errors.New("storage event schema ownership mismatch")
	}
	if _, err = tx.Exec(ctx, objectEventsSchema, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	var pending int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM neon_storage.object_events WHERE branch_id=$1 AND timeline_id=$2 AND state='pending'`, p.BranchID, timeline).Scan(&pending); err != nil {
		return err
	}
	if pending >= 512 {
		return errors.New("storage event outbox full")
	}
	event := storageEventEnvelope{ID: "soe_" + randomToken(16), Project: p.ProjectID, Branch: p.BranchID, Timeline: timeline, KeyVersion: s.functions.input.KeyVersion, Created: time.Now().UTC(), Data: storageCreatedData{Bucket: bucket, Key: key, SHA256: digest, Size: size}, Targets: targets}
	raw, err := json.Marshal(event)
	if err != nil || len(raw) > 60<<10 {
		return errors.New("storage event envelope limit")
	}
	// The native lock is already held before this marker commits. An exporter
	// cannot observe an empty pre-publication catalog then clear this generation.
	_, err = s.db.Exec(ctx, `INSERT INTO storage_event_branches(branch_id,project_id,endpoint_id,database_name,timeline_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT(branch_id) DO UPDATE SET endpoint_id=$3,database_name=$4,timeline_id=$5,generation=storage_event_branches.generation+1,dirty=true,error_code=NULL,updated_at=clock_timestamp()`, p.BranchID, p.ProjectID, p.EndpointID, p.Spec.Database, timeline)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO neon_storage.object_events(id,project_id,branch_id,timeline_id,envelope,mac,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, event.ID, event.Project, event.Branch, event.Timeline, raw, storageEventMAC(s.functions.key, event), event.Created)
	return err
}

type storagePublicationKey struct{}
type storagePublication func(context.Context, pgx.Tx, string, string, string, int64) error

func (s *server) storagePublicationContext(ctx context.Context, p objectStoragePayload) context.Context {
	return context.WithValue(ctx, storagePublicationKey{}, storagePublication(func(ctx context.Context, tx pgx.Tx, bucket, key, digest string, size int64) error {
		return s.publishStorageFunctionEvent(ctx, tx, p, bucket, key, digest, size)
	}))
}
func publishStorageCopyEvent(ctx context.Context, tx pgx.Tx, bucket, key, digest string, size int64) error {
	if fn, ok := ctx.Value(storagePublicationKey{}).(storagePublication); ok {
		return fn(ctx, tx, bucket, key, digest, size)
	}
	return nil
}

func validStorageEventPrefix(prefix string) bool {
	if prefix == "" {
		return true
	}
	if len(prefix) > 1024 {
		return false
	}
	if strings.HasSuffix(prefix, "/") {
		return validStorageKey(prefix + "x")
	}
	return validStorageKey(prefix)
}
