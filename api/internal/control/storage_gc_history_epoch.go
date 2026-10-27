package control

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed assets/storage-gc-history.sql
var storageGCHistorySQL string

var errStorageGCHistoryIdentity = errors.New("Storage GC history installation or native scope rejected")

type storageGCHistoryScope struct {
	TenantID    string `json:"tenant_id"`
	TimelineID  string `json:"timeline_id"`
	DatabaseOID uint32 `json:"database_oid"`
	Database    string `json:"database"`
}

func (v storageGCHistoryScope) valid() bool {
	return (catalogNativeScope{TenantID: v.TenantID, TimelineID: v.TimelineID}).valid() && v.DatabaseOID > 0 && validPGIdentifier(v.Database)
}

type storageGCHistoryDirectory struct {
	Scope               storageGCHistoryScope `json:"scope"`
	Incarnation         string                `json:"incarnation"`
	SchemaSHA256        string                `json:"schema_sha256"`
	Epoch               int64                 `json:"epoch"`
	ObjectReferences    int64                 `json:"object_references"`
	MultipartReferences int64                 `json:"multipart_references"`
	// Observation upper bound only; never a mutation/transaction commit LSN.
	ObservationLSN string `json:"observation_lsn"`
	RetirementID   string `json:"retirement_id"`
}

// A directory lease owns actual PostgreSQL SHARE table locks. They stay held
// until Close, covering every current reference mutation including COPY/import,
// staging parts, direct DML, TRUNCATE and relation DDL. No snapshot bool is a lock.
type storageGCHistoryDirectoryLease interface {
	Observation() storageGCHistoryDirectory
	Check(context.Context) error
	Quarantine(context.Context, string) error
	Close(context.Context) error
}
type storageGCHistorySQLLease struct {
	tx          pgx.Tx
	observation storageGCHistoryDirectory
	key         string
	closed      bool
}

func (v *storageGCHistorySQLLease) Observation() storageGCHistoryDirectory { return v.observation }
func (v *storageGCHistorySQLLease) Check(ctx context.Context) error {
	if v.closed {
		return errStorageGCHistoryIdentity
	}
	_, err := v.tx.Exec(ctx, `SELECT 1`)
	return err
}
func (v *storageGCHistorySQLLease) Close(ctx context.Context) error {
	if v.closed {
		return nil
	}
	v.closed = true
	return v.tx.Rollback(ctx)
}
func (v *storageGCHistorySQLLease) Quarantine(ctx context.Context, retirement string) error {
	if v.closed || !regexp.MustCompile(`^sgh_[a-f0-9]{24}$`).MatchString(retirement) || v.observation.ObjectReferences != 0 || v.observation.MultipartReferences != 0 {
		return errStorageGCHistoryIdentity
	}
	if _, err := v.tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	if _, err := v.tx.Exec(ctx, `INSERT INTO neon_storage.gc_history_retirements(blob_key,retirement_id) VALUES($1,$2) ON CONFLICT(blob_key) DO NOTHING`, v.key, retirement); err != nil {
		return err
	}
	var stored string
	if err := v.tx.QueryRow(ctx, `SELECT retirement_id FROM neon_storage.gc_history_retirements WHERE blob_key=$1`, v.key).Scan(&stored); err != nil || stored != retirement {
		return errStorageGCHistoryIdentity
	}
	if err := v.tx.Commit(ctx); err != nil {
		return err
	}
	v.closed = true
	v.observation.RetirementID = stored
	return nil
}

// Catalog attestation excludes table DATA (epochs necessarily change), but
// includes all protected relations/columns/constraints/ACLs/functions/triggers.
// New version-history/reference tables or disabled/rewritten triggers invalidate
// the installation instead of being silently excluded from the reference scan.
const storageGCHistoryCatalogSQL = `SELECT jsonb_build_object(
 'relations',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.relname) FROM (
 SELECT c.relname,c.relkind,c.relowner,c.relacl,c.relrowsecurity,c.relforcerowsecurity,c.relhasrules,c.relhassubclass,
  (SELECT jsonb_agg(jsonb_build_array(a.attname,a.atttypid,a.atttypmod,a.attnotnull,a.attacl) ORDER BY a.attnum) FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped) AS columns,
  (SELECT jsonb_agg(pg_get_constraintdef(k.oid,true) ORDER BY k.conname) FROM pg_constraint k WHERE k.conrelid=c.oid) AS constraints,
  (SELECT jsonb_agg(jsonb_build_array(t.tgname,t.tgenabled,pg_get_triggerdef(t.oid,true)) ORDER BY t.tgname) FROM pg_trigger t WHERE t.tgrelid=c.oid AND NOT t.tgisinternal) AS triggers
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='neon_storage' AND c.relkind IN ('r','p','v','m','f')
 )x),'[]'::jsonb),
 'functions',COALESCE((SELECT jsonb_agg(jsonb_build_array(p.proname,pg_get_function_identity_arguments(p.oid),p.proowner,p.proacl,p.prosecdef,p.proconfig,p.prosrc) ORDER BY p.proname,p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='neon_storage'),'[]'::jsonb),
 'schema',(SELECT jsonb_build_array(n.nspowner,n.nspacl,obj_description(n.oid,'pg_namespace')) FROM pg_namespace n WHERE n.nspname='neon_storage'))::text`

func storageGCHistoryCatalogDigest(ctx context.Context, tx pgx.Tx) (string, error) {
	var text string
	if err := tx.QueryRow(ctx, storageGCHistoryCatalogSQL).Scan(&text); err != nil {
		return "", err
	}
	if len(text) > 1<<20 {
		return "", errStorageGCHistoryIdentity
	}
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:]), nil
}

func storageGCHistoryShapeCheck(ctx context.Context, tx pgx.Tx, installed bool) error {
	expected := map[string]string{
		"buckets":           "name access created_at cors settings settings_version",
		"objects":           "bucket key blob_key sha256 size content_type updated_at metadata tags response_headers part_layout checksums checksum_policy",
		"installations":     "branch_id generation format_version",
		"multipart_uploads": "id branch_id principal bucket key content_type metadata expected_sha256 expected_size request_key request_hash state completion_hash result_sha256 result_size created_at updated_at tags response_headers checksum_algorithm checksum_type result_checksum_policy",
		"multipart_parts":   "upload_id part_number blob_key sha256 etag size updated_at checksums",
		"object_events":     "id project_id branch_id timeline_id envelope mac state error_code created_at exported_at",
		"setting_mutations": "branch_id actor_id key_hash request_hash response etag created_at",
	}
	if installed {
		expected["gc_history_installation"] = "singleton version base_format project_id incarnation schema_sha256"
		expected["gc_history_epochs"] = "blob_key epoch"
		expected["gc_history_retirements"] = "blob_key retirement_id"
	}
	rows, err := tx.Query(ctx, `SELECT c.relname,c.relkind::text,array_agg(a.attname::text ORDER BY a.attname COLLATE "C"),c.relowner=n.nspowner AND NOT c.relrowsecurity AND NOT c.relforcerowsecurity AND NOT c.relhasrules AND NOT EXISTS(SELECT 1 FROM pg_inherits WHERE inhparent=c.oid OR inhrelid=c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped WHERE n.nspname='neon_storage' AND c.relkind IN ('r','p','v','m','f') GROUP BY c.oid,c.relname,c.relkind,c.relowner,n.nspowner,c.relrowsecurity,c.relforcerowsecurity,c.relhasrules ORDER BY c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name, kind string
		var columns []string
		var owned bool
		if err = rows.Scan(&name, &kind, &columns, &owned); err != nil {
			return err
		}
		want, ok := expected[name]
		if !ok || kind != "r" || !owned {
			return errStorageGCHistoryIdentity
		}
		fields := strings.Fields(want)
		sort.Strings(fields)
		if strings.Join(fields, " ") != strings.Join(columns, " ") {
			return errStorageGCHistoryIdentity
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count != len(expected) {
		return errStorageGCHistoryIdentity
	}
	return nil
}

// Explicit opt-in installer. Native format 7 enable/replay NEVER calls this.
// Existing installations are attested/reused, never overwritten or reset.
func installStorageGCHistoryEpochs(ctx context.Context, conn *pgx.Conn, project string) (string, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,922743))`, project); err != nil {
		return "", err
	}
	var marker, owner string
	if err = tx.QueryRow(ctx, `SELECT COALESCE(obj_description(oid,'pg_namespace'),''),pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='neon_storage'`).Scan(&marker, &owner); err != nil || marker != "neon-control:object-storage:"+project {
		return "", errStorageGCHistoryIdentity
	}
	var current string
	var format int
	if err = tx.QueryRow(ctx, `SELECT current_user,(SELECT max(COALESCE((to_jsonb(i)->>'format_version')::int,1)) FROM neon_storage.installations i)`).Scan(&current, &format); err != nil || owner != current || format != 7 {
		return "", errStorageGCHistoryIdentity
	}
	var installed bool
	if err = tx.QueryRow(ctx, `SELECT to_regclass('neon_storage.gc_history_installation') IS NOT NULL`).Scan(&installed); err != nil {
		return "", err
	}
	if err = storageGCHistoryShapeCheck(ctx, tx, installed); err != nil {
		return "", err
	}
	if installed {
		var id, stored, original string
		var version, base int
		if err = tx.QueryRow(ctx, `SELECT incarnation,schema_sha256,project_id,version,base_format FROM neon_storage.gc_history_installation WHERE singleton`).Scan(&id, &stored, &original, &version, &base); err != nil {
			return "", errStorageGCHistoryIdentity
		}
		actual, e := storageGCHistoryCatalogDigest(ctx, tx)
		if e != nil || actual != stored || original != project || version != 1 || base != 7 {
			return "", errStorageGCHistoryIdentity
		}
		return id, tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, storageGCHistorySQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		return "", err
	}
	if err = storageGCHistoryShapeCheck(ctx, tx, true); err != nil {
		return "", err
	}
	if err = storageGCHistoryPrivilegeCheck(ctx, tx); err != nil {
		return "", err
	}
	digest, err := storageGCHistoryCatalogDigest(ctx, tx)
	if err != nil {
		return "", err
	}
	id := newID("sge_")
	if _, err = tx.Exec(ctx, `INSERT INTO neon_storage.gc_history_installation(singleton,version,base_format,project_id,incarnation,schema_sha256) VALUES(true,1,7,$1,$2,$3)`, project, id, digest); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func storageGCHistoryPrivilegeCheck(ctx context.Context, tx pgx.Tx) error {
	var unsafe bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl,acldefault('r',c.relowner))) a WHERE n.nspname='neon_storage' AND c.relname IN ('gc_history_installation','gc_history_epochs','gc_history_retirements') AND a.grantee<>c.relowner AND a.privilege_type IN ('INSERT','UPDATE','DELETE','TRUNCATE','TRIGGER')) OR EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace CROSS JOIN LATERAL aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE n.nspname='neon_storage' AND p.proname LIKE 'gc_history_%' AND a.grantee<>p.proowner AND a.privilege_type='EXECUTE')`).Scan(&unsafe)
	if err != nil {
		return err
	}
	if unsafe {
		return errStorageGCHistoryIdentity
	}
	return nil
}

func openStorageGCHistoryDirectory(ctx context.Context, conn *pgx.Conn, scope storageGCHistoryScope, project, key string) (storageGCHistoryDirectoryLease, error) {
	if !scope.valid() || !storageGCBlobKey.MatchString(key) || !strings.HasPrefix(key, project+"/") {
		return nil, errStorageGCHistoryIdentity
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = tx.Rollback(context.Background())
		}
	}()
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='10s'; SET LOCAL row_security=off; LOCK TABLE neon_storage.objects,neon_storage.multipart_parts,neon_storage.gc_history_epochs,neon_storage.gc_history_installation IN SHARE MODE`, pgx.QueryExecModeSimpleProtocol); err != nil {
		return nil, err
	}
	var actual storageGCHistoryScope
	if err = tx.QueryRow(ctx, `SELECT current_setting('neon.tenant_id',true),current_setting('neon.timeline_id',true),oid,datname FROM pg_database WHERE datname=current_database()`).Scan(&actual.TenantID, &actual.TimelineID, &actual.DatabaseOID, &actual.Database); err != nil || actual != scope {
		return nil, errStorageGCHistoryIdentity
	}
	var observed storageGCHistoryDirectory
	observed.Scope = scope
	var installedProject string
	var version, base, format int
	if err = tx.QueryRow(ctx, `SELECT incarnation,schema_sha256,project_id,version,base_format,(SELECT max((to_jsonb(i)->>'format_version')::int) FROM neon_storage.installations i) FROM neon_storage.gc_history_installation WHERE singleton`).Scan(&observed.Incarnation, &observed.SchemaSHA256, &installedProject, &version, &base, &format); err != nil || installedProject != project || version != 1 || base != 7 || format != 7 {
		return nil, errStorageGCHistoryIdentity
	}
	digest, e := storageGCHistoryCatalogDigest(ctx, tx)
	if e != nil || digest != observed.SchemaSHA256 {
		return nil, errStorageGCHistoryIdentity
	}
	if err = storageGCHistoryPrivilegeCheck(ctx, tx); err != nil {
		return nil, err
	}
	if err = storageGCHistoryShapeCheck(ctx, tx, true); err != nil {
		return nil, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT epoch FROM neon_storage.gc_history_epochs WHERE blob_key=$1),0),(SELECT count(*) FROM neon_storage.objects WHERE blob_key=$1),(SELECT count(*) FROM neon_storage.multipart_parts WHERE blob_key=$1),pg_current_wal_insert_lsn()::text,COALESCE((SELECT retirement_id FROM neon_storage.gc_history_retirements WHERE blob_key=$1),'')`, key).Scan(&observed.Epoch, &observed.ObjectReferences, &observed.MultipartReferences, &observed.ObservationLSN, &observed.RetirementID); err != nil {
		return nil, err
	}
	if _, err = parseLSN(observed.ObservationLSN); err != nil {
		return nil, err
	}
	failed = false
	return &storageGCHistorySQLLease{tx: tx, observation: observed, key: key}, nil
}
