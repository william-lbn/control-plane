package control

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

type storageBucketSettingsInput struct {
	Access   string                 `json:"access"`
	Settings *storageBucketSettings `json:"settings"`
}

func replayStorageSettings(ctx context.Context, tx pgx.Tx, branch, actor, key, hash string) (record, string, error) {
	var original, etag string
	var response record
	e := tx.QueryRow(ctx, `SELECT request_hash,response,etag FROM neon_storage.setting_mutations WHERE branch_id=$1 AND actor_id=$2 AND key_hash=$3`, branch, actor, key).Scan(&original, &response, &etag)
	if isNoRows(e) {
		return nil, "", nil
	}
	if e != nil {
		return nil, "", e
	}
	if original != hash {
		return nil, "", &storageMutationFault{409, "IdempotencyConflict"}
	}
	return response, etag, nil
}
func persistStorageSettings(ctx context.Context, tx pgx.Tx, branch, actor, key, hash, etag string, response record) error {
	_, e := tx.Exec(ctx, `INSERT INTO neon_storage.setting_mutations(branch_id,actor_id,key_hash,request_hash,response,etag) VALUES($1,$2,$3,$4,$5,$6)`, branch, actor, key, hash, response, etag)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func nativeStorageBucketSettings(ctx context.Context, conn *pgx.Conn, p objectStoragePayload, bucket, actor, key, hash, expected string, input *storageBucketSettingsInput) (record, string, bool, error) {
	if !storageBucketName.MatchString(bucket) || input != nil && (input.Settings == nil || input.Access != "private" && input.Access != "public_read" || actor == "" || key == "" || hash == "") {
		return nil, "", false, &storageMutationFault{422, "InvalidSettings"}
	}
	tx, e := multipartTx(ctx, conn)
	if e != nil {
		return nil, "", false, e
	}
	defer tx.Rollback(ctx)
	if input != nil {
		prior, etag, e := replayStorageSettings(ctx, tx, p.BranchID, actor, key, hash)
		if e != nil || prior != nil {
			return prior, etag, prior != nil, e
		}
	}
	var access string
	var version int64
	var settings storageBucketSettings
	e = tx.QueryRow(ctx, `SELECT access,settings_version,settings FROM neon_storage.buckets WHERE name=$1`, bucket).Scan(&access, &version, &settings)
	if e != nil {
		return nil, "", false, e
	}
	settings, e = normalizeStorageSettings(settings)
	if e != nil {
		return nil, "", false, e
	}
	etag := storageSettingsETag(p.BranchID, bucket, access, version, settings)
	if input != nil {
		if expected == "" {
			return nil, "", false, &storageMutationFault{428, "SettingsVersionRequired"}
		}
		if expected != etag {
			return nil, "", false, &storageMutationFault{412, "PreconditionFailed"}
		}
		desired, e := normalizeStorageSettings(*input.Settings)
		if e != nil {
			return nil, "", false, &storageMutationFault{422, "InvalidSettings"}
		}
		if input.Access == "public_read" && access != "public_read" && (desired.publicBlock().BlockPublicACLs || desired.publicBlock().BlockPublicPolicy || settings.publicBlock().BlockPublicACLs || settings.publicBlock().BlockPublicPolicy) {
			return nil, "", false, &storageMutationFault{409, "PublicAccessBlocked"}
		}
		settings, access, version = desired, input.Access, version+1
		if _, e = tx.Exec(ctx, `UPDATE neon_storage.buckets SET access=$2,settings=$3,settings_version=$4 WHERE name=$1`, bucket, access, settings, version); e != nil {
			return nil, "", false, e
		}
	}
	response := storageSettingsReceipt(p.BranchID, bucket, access, version, settings)
	etag = storageSettingsETag(p.BranchID, bucket, access, version, settings)
	if input != nil {
		e = persistStorageSettings(ctx, tx, p.BranchID, actor, key, hash, etag, response)
	}
	return response, etag, false, e
}
func nativeStorageObjectSettings(ctx context.Context, conn *pgx.Conn, p objectStoragePayload, bucket, objectKey, actor, key, hash, expected string, input *storageObjectSettings) (record, string, bool, error) {
	if !storageBucketName.MatchString(bucket) || !validStorageKey(objectKey) || input != nil && (actor == "" || key == "" || hash == "") {
		return nil, "", false, &storageMutationFault{422, "InvalidSettings"}
	}
	tx, e := multipartTx(ctx, conn)
	if e != nil {
		return nil, "", false, e
	}
	defer tx.Rollback(ctx)
	if input != nil {
		prior, etag, e := replayStorageSettings(ctx, tx, p.BranchID, actor, key, hash)
		if e != nil || prior != nil {
			return prior, etag, prior != nil, e
		}
	}
	var blob, digest string
	var settings storageObjectSettings
	e = tx.QueryRow(ctx, `SELECT blob_key,sha256,tags,response_headers FROM neon_storage.objects WHERE bucket=$1 AND key=$2`, bucket, objectKey).Scan(&blob, &digest, &settings.Tags, &settings.ResponseHeaders)
	if e != nil {
		return nil, "", false, e
	}
	if !strings.HasPrefix(blob, p.ProjectID+"/") {
		return nil, "", false, &storageMutationFault{409, "ObjectIdentityMismatch"}
	}
	settings, e = normalizeStorageObjectSettings(settings)
	if e != nil {
		return nil, "", false, e
	}
	etag := storageObjectSettingsETag(p.BranchID, bucket, objectKey, blob, digest, settings)
	if input != nil {
		if expected == "" {
			return nil, "", false, &storageMutationFault{428, "SettingsVersionRequired"}
		}
		if expected != etag {
			return nil, "", false, &storageMutationFault{412, "PreconditionFailed"}
		}
		settings, e = normalizeStorageObjectSettings(*input)
		if e != nil {
			return nil, "", false, &storageMutationFault{422, "InvalidSettings"}
		}
		if _, e = tx.Exec(ctx, `UPDATE neon_storage.objects SET tags=$3,response_headers=$4 WHERE bucket=$1 AND key=$2`, bucket, objectKey, settings.Tags, settings.ResponseHeaders); e != nil {
			return nil, "", false, e
		}
	}
	response := storageObjectSettingsReceipt(p.BranchID, bucket, objectKey, digest, settings)
	etag = storageObjectSettingsETag(p.BranchID, bucket, objectKey, blob, digest, settings)
	if input != nil {
		e = persistStorageSettings(ctx, tx, p.BranchID, actor, key, hash, etag, response)
	}
	return response, etag, false, e
}
