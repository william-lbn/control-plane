package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type storageGCConfig struct {
	Backing           storageBlobConfig `json:"backing"`
	Operators         []string          `json:"operator_ids"`
	Projects          []string          `json:"allowed_project_ids"`
	MinimumAgeSeconds int64             `json:"minimum_age_seconds"`
}

type storageGCBacking interface {
	inspect(context.Context, string) (minio.ObjectInfo, error)
	remove(context.Context, string) error
	versioningDisabled(context.Context) error
}

type storageGCClient struct {
	client *minio.Client
	bucket string
}

type storageGCDeleter struct {
	backing             storageGCBacking
	operators, projects map[string]bool
	minimumAge          time.Duration
}

func loadStorageGCDeleter(ordinary *storageBlobs) (*storageGCDeleter, error) {
	path := os.Getenv("NEON_OBJECT_STORAGE_GC_CONFIG_FILE")
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("approved storage GC configuration unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(raw) > 65536 {
		return nil, errors.New("invalid approved storage GC configuration")
	}
	return parseStorageGCDeleter(raw, ordinary)
}

func parseStorageGCDeleter(raw []byte, ordinary *storageBlobs) (*storageGCDeleter, error) {
	var c storageGCConfig
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	invalid := errors.New("invalid approved storage GC configuration")
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || ordinary == nil || ordinary.client == nil || c.MinimumAgeSeconds < 60 || c.MinimumAgeSeconds > 30*86400 || len(c.Operators) < 1 || len(c.Operators) > 16 || len(c.Projects) < 1 || len(c.Projects) > 128 {
		return nil, invalid
	}
	u, err := url.Parse(c.Backing.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && c.Backing.LabHTTP)) || c.Backing.Region == "" || c.Backing.Bucket != ordinary.bucket || u.Host != ordinary.client.EndpointURL().Host || u.Scheme != ordinary.client.EndpointURL().Scheme || !regexp.MustCompile(`^neon-product-[a-z0-9-]{1,40}$`).MatchString(c.Backing.Bucket) || len(c.Backing.AccessKey) < 8 || len(c.Backing.SecretKey) < 16 {
		return nil, invalid
	}
	result := &storageGCDeleter{operators: map[string]bool{}, projects: map[string]bool{}, minimumAge: time.Duration(c.MinimumAgeSeconds) * time.Second}
	for _, id := range c.Operators {
		if !aiProviderUserID.MatchString(id) || result.operators[id] {
			return nil, invalid
		}
		result.operators[id] = true
	}
	for _, id := range c.Projects {
		if !regexp.MustCompile(`^prj_[a-f0-9]{16,32}$`).MatchString(id) || result.projects[id] {
			return nil, invalid
		}
		result.projects[id] = true
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 15 * time.Second
	client, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(c.Backing.AccessKey, c.Backing.SecretKey, ""), Secure: u.Scheme == "https", Region: c.Backing.Region, Transport: transport, BucketLookup: minio.BucketLookupPath})
	if err != nil {
		return nil, invalid
	}
	result.backing = &storageGCClient{client: client, bucket: c.Backing.Bucket}
	return result, nil
}

func (c *storageGCClient) inspect(ctx context.Context, key string) (minio.ObjectInfo, error) {
	return c.client.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
}
func (c *storageGCClient) remove(ctx context.Context, key string) error {
	return c.client.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{})
}
func (c *storageGCClient) versioningDisabled(ctx context.Context) error {
	v, err := c.client.GetBucketVersioning(ctx, c.bucket)
	if err != nil || v.Status != "" {
		// Suspended buckets can still contain retained versions. This release
		// collects only never-versioned product stores; no delete markers.
		return errors.New("storage GC requires a verified unversioned product bucket")
	}
	return nil
}

func (s *server) storageGCOperator(w http.ResponseWriter, r *http.Request) (*storageGCJournal, bool) {
	if s.storage == nil || s.storage.gcJournal == nil || s.storage.gcJournal.deleter == nil {
		fail(w, r, 503, "storage_gc_disabled", "Storage GC requires a separately approved deletion credential and project policy")
		return nil, false
	}
	j := s.storage.gcJournal
	u := userFrom(r)
	if u.APIKey || !j.deleter.operators[u.ID] || !j.deleter.projects[r.PathValue("project")] {
		fail(w, r, 403, "storage_gc_operator_required", "An approved operator session and exact project scope are required")
		return nil, false
	}
	var active bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND NOT disabled)`, u.ID).Scan(&active); err != nil || !active {
		fail(w, r, 403, "storage_gc_operator_required", "Active approved operator session required")
		return nil, false
	}
	return j, true
}

// Plan accepts only terminal never_published allocation evidence. Inventory or
// client-supplied absence/retention claims never promote another allocation.
func (s *server) storageGCPlan(w http.ResponseWriter, r *http.Request) {
	j, ok := s.storageGCOperator(w, r)
	if !ok {
		return
	}
	var input struct {
		Limit int `json:"limit"`
	}
	if readJSON(r, &input) != nil || input.Limit < 1 || input.Limit > 100 {
		fail(w, r, 422, "invalid_gc_plan", "A limit from 1 to 100 is required")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		storageGCFailure(w, r)
		return
	}
	defer tx.Rollback(ctx)
	actor, project := userFrom(r).ID, r.PathValue("project")
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('storage-gc-plan:'||$1||':'||$2,0))`, actor, keyHash(key))
	hash := s.createRequestHash("storage-gc-plan:"+project+":"+j.storeID, input)
	var id, previous string
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT id,request_hash FROM storage_gc_runs WHERE actor_id=$1 AND key_hash=$2`, actor, keyHash(key)).Scan(&id, &previous)
	}
	if err == nil {
		if previous != hash {
			fail(w, r, 409, "idempotency_conflict", "Key belongs to different GC input")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
	} else if isNoRows(err) {
		id = newID("sgr_")
		_, err = tx.Exec(ctx, `INSERT INTO storage_gc_runs(id,store_id,project_id,actor_id,key_hash,request_hash) VALUES($1,$2,$3,$4,$5,$6)`, id, j.storeID, project, actor, keyHash(key), hash)
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO storage_gc_items(run_id,store_id,project_id,blob_key) SELECT $1,store_id,project_id,blob_key FROM storage_gc_allocations a WHERE store_id=$2 AND project_id=$3 AND state='never_published' AND resolved_at<=clock_timestamp()-($4::bigint*interval '1 second') AND NOT EXISTS(SELECT 1 FROM storage_gc_items i WHERE i.store_id=a.store_id AND i.blob_key=a.blob_key AND i.state='deleted') ORDER BY created_at,allocation_id LIMIT $5`, id, j.storeID, project, int64(j.deleter.minimumAge/time.Second), input.Limit)
		}
	}
	if err != nil || tx.Commit(ctx) != nil {
		storageGCFailure(w, r)
		return
	}
	s.audit(ctx, actor, "plan_storage_gc", "project", project, "succeeded", requestID(r))
	s.storageGCRespond(w, r, id, j.storeID)
}

func storageGCFailure(w http.ResponseWriter, r *http.Request) {
	fail(w, r, 503, "storage_gc_unavailable", "Storage GC evidence is unavailable; unknown results remain protected")
}

func (s *server) storageGCRespond(w http.ResponseWriter, r *http.Request, id, store string) {
	value, err := s.one(r.Context(), `SELECT id,project_id,actor_id,created_at FROM storage_gc_runs WHERE id=$1 AND project_id=$2 AND store_id=$3`, id, r.PathValue("project"), store)
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "GC plan not found")
		return
	}
	if err != nil {
		storageGCFailure(w, r)
		return
	}
	items, err := s.many(r.Context(), `SELECT a.allocation_id,a.size,i.state,i.reason,i.attempts,i.updated_at FROM storage_gc_items i JOIN storage_gc_allocations a USING(store_id,blob_key) WHERE i.run_id=$1 ORDER BY a.allocation_id`, id)
	if err != nil {
		storageGCFailure(w, r)
		return
	}
	value["items"] = items
	attempts, err := s.many(r.Context(), `SELECT a.allocation_id,v.attempt,v.phase,v.actor_id,v.state,v.reason,v.recorded_at FROM storage_gc_attempts v JOIN storage_gc_items i USING(run_id,blob_key) JOIN storage_gc_allocations a USING(store_id,blob_key) WHERE v.run_id=$1 ORDER BY a.allocation_id,v.attempt,v.recorded_at`, id)
	if err != nil {
		storageGCFailure(w, r)
		return
	}
	value["attempt_history"] = attempts
	value["scope"] = "never_published_only"
	value["published_history_gc"] = "held_native_proof_unavailable"
	jsonResponse(w, 200, value)
}

func (s *server) storageGCRead(w http.ResponseWriter, r *http.Request) {
	if j, ok := s.storageGCOperator(w, r); ok {
		s.storageGCRespond(w, r, r.PathValue("gcRun"), j.storeID)
	}
}

func (s *server) storageGCExecute(w http.ResponseWriter, r *http.Request) {
	j, ok := s.storageGCOperator(w, r)
	if !ok {
		return
	}
	var input struct {
		ConfirmRun string `json:"confirm_run"`
	}
	id := r.PathValue("gcRun")
	if readJSON(r, &input) != nil || input.ConfirmRun != id || !regexp.MustCompile(`^sgr_[a-f0-9]{24}$`).MatchString(id) {
		fail(w, r, 422, "gc_plan_confirmation_required", "Confirm the exact previously reviewed GC run ID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	var exists bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM storage_gc_runs WHERE id=$1 AND project_id=$2 AND store_id=$3)`, id, r.PathValue("project"), j.storeID).Scan(&exists)
	if err != nil {
		storageGCFailure(w, r)
		return
	}
	if !exists {
		fail(w, r, 404, "not_found", "GC plan not found")
		return
	}
	if j.deleter.backing.versioningDisabled(ctx) != nil {
		fail(w, r, 409, "storage_gc_versioning_unverified", "GC requires verified disabled bucket versioning")
		return
	}
	rows, err := s.db.Query(ctx, `SELECT blob_key FROM storage_gc_items WHERE run_id=$1 AND state IN ('planned','delete_unknown','absent') ORDER BY blob_key`, id)
	if err != nil {
		storageGCFailure(w, r)
		return
	}
	keys := []string{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			break
		}
		keys = append(keys, key)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		storageGCFailure(w, r)
		return
	}
	for _, key := range keys {
		if err = j.collect(ctx, id, key, userFrom(r).ID); err != nil {
			storageGCFailure(w, r)
			return
		}
	}
	s.audit(ctx, userFrom(r).ID, "execute_storage_gc", "project", r.PathValue("project"), "succeeded", requestID(r))
	s.storageGCRespond(w, r, id, j.storeID)
}

func (j *storageGCJournal) collect(ctx context.Context, run, key, actor string) error {
	tx, err := j.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var activeActor string
	if err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 AND NOT disabled FOR SHARE`, actor).Scan(&activeActor); err != nil {
		return err
	}
	var allocation, allocationState, itemState string
	var size int64
	var attempts int
	var oldEnough bool
	// One exact allocation lock serializes different plans and replicas, held
	// across HEAD/DELETE. Native publication cannot use never_published keys.
	err = tx.QueryRow(ctx, `SELECT allocation_id,size,state,resolved_at<=clock_timestamp()-($3::bigint*interval '1 second') FROM storage_gc_allocations WHERE store_id=$1 AND blob_key=$2 FOR NO KEY UPDATE`, j.storeID, key, int64(j.deleter.minimumAge/time.Second)).Scan(&allocation, &size, &allocationState, &oldEnough)
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT state FROM storage_gc_items WHERE run_id=$1 AND store_id=$2 AND blob_key=$3 FOR NO KEY UPDATE`, run, j.storeID, key).Scan(&itemState)
	if err != nil {
		return err
	}
	if itemState == "deleted" || itemState == "held" {
		return nil
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(attempt),0)+1 FROM storage_gc_attempts WHERE run_id=$1 AND blob_key=$2`, run, key).Scan(&attempts); err != nil {
		return err
	}
	// Commit a separate append-only intent BEFORE any backing request. The
	// item/allocation NO KEY UPDATE locks serialize collectors while allowing
	// this connection's FK KEY SHARE checks. Cancellation/unknown commit leaves
	// durable unknown evidence and never grants permission to issue DELETE.
	_, err = j.allocationDB().Exec(ctx, `INSERT INTO storage_gc_attempts(run_id,blob_key,attempt,phase,actor_id,state,reason) VALUES($1,$2,$3,'intent',$4,'delete_unknown','backing_operation_intent')`, run, key, attempts, actor)
	if err != nil {
		return err
	}
	state, reason := "held", "allocation_not_collectable"
	if storageGCBlobKey.MatchString(key) && allocationState == "never_published" && oldEnough {
		state, reason = storageGCCollectObject(ctx, j.deleter.backing, key, allocation, size)
	}
	_, err = tx.Exec(ctx, `UPDATE storage_gc_items SET state=$3,reason=$4,attempts=$5,updated_at=clock_timestamp() WHERE run_id=$1 AND blob_key=$2`, run, key, state, reason, attempts)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO storage_gc_attempts(run_id,blob_key,attempt,phase,actor_id,state,reason) VALUES($1,$2,$3,'result',$4,$5,$6)`, run, key, attempts, actor, state, reason)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func storageGCCollectObject(ctx context.Context, backing storageGCBacking, key, allocation string, size int64) (string, string) {
	info, err := backing.inspect(ctx, key)
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return "absent", "backing_absent_observed"
		}
		return "delete_unknown", "backing_inspection_unknown"
	}
	marker, markers := "", 0
	for name, value := range info.UserMetadata {
		if strings.EqualFold(name, storageGCMetadata) {
			marker = value
			markers++
		}
	}
	if markers != 1 || marker != allocation || info.Size != size || info.ETag == "" || info.VersionID != "" {
		return "held", "backing_identity_or_version_conflict"
	}
	if backing.remove(ctx, key) != nil {
		return "delete_unknown", "backing_delete_unknown"
	}
	// A lost DELETE response is reconciled by a later exact HEAD. Never report
	// bytes reclaimed just because DELETE was accepted or an attempt timed out.
	_, err = backing.inspect(ctx, key)
	if err == nil {
		return "delete_unknown", "backing_still_present"
	}
	if minio.ToErrorResponse(err).Code != "NoSuchKey" {
		return "delete_unknown", "backing_delete_verification_unknown"
	}
	return "deleted", "backing_delete_verified"
}
