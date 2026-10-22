package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed assets/auth-factor-rebind.sql
var managedAuthFactorRebindSchema string

type managedAuthFactorProof struct {
	Version                 int    `json:"version"`
	ProjectID               string `json:"project_id"`
	BranchID                string `json:"branch_id"`
	Database                string `json:"database"`
	SourceBranchID          string `json:"source_branch_id"`
	SourceGeneration        int64  `json:"source_generation"`
	TargetGeneration        int64  `json:"target_generation"`
	OperationID             string `json:"operation_id"`
	Migrated                int    `json:"migrated_factors"`
	PendingRemoved          int    `json:"pending_enrollments_removed"`
	SourceDigest            string `json:"source_ciphertext_digest"`
	TargetDigest            string `json:"target_ciphertext_digest"`
	RecoveryCodesReset      bool   `json:"recovery_codes_reset"`
	InheritedLoginsDisabled int    `json:"inherited_logins_disabled"`
}

func (s *server) managedAuthFactorMAC(p managedAuthFactorProof) string {
	raw, _ := json.Marshal(p)
	mac := hmac.New(sha256.New, s.idempotencyKey)
	mac.Write([]byte("neon.auth.factor-rebind.receipt.v1\x00"))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *server) validManagedAuthFactorProof(proof managedAuthFactorProof, mac string, p managedAuthPayload) bool {
	return len(s.idempotencyKey) >= 32 && proof.Version == 1 && proof.ProjectID == p.ProjectID && proof.BranchID == p.BranchID && proof.Database == p.Spec.Database && proof.SourceBranchID == p.FactorSourceBranchID && proof.SourceGeneration > 0 && proof.TargetGeneration > 0 && proof.TargetGeneration <= p.Generation && proof.OperationID != "" && proof.Migrated >= 0 && proof.PendingRemoved >= 0 && proof.InheritedLoginsDisabled > 0 && len(proof.SourceDigest) == 64 && len(proof.TargetDigest) == 64 && proof.RecoveryCodesReset && hmac.Equal([]byte(mac), []byte(s.managedAuthFactorMAC(proof)))
}

// Forked SQL roles must not let a source service's database password log in to
// the target timeline. Keep the roles and their ACLs, but disable copied logins.
func inheritedManagedAuthLoginsTx(ctx context.Context, tx pgx.Tx, p managedAuthPayload, write bool) (int, error) {
	rows, err := tx.Query(ctx, `SELECT branch_id FROM neon_auth.control_installations WHERE branch_id<>$1 ORDER BY branch_id`, p.BranchID)
	if err != nil {
		return 0, err
	}
	var branches []string
	for rows.Next() {
		var branch string
		if err = rows.Scan(&branch); err != nil {
			rows.Close()
			return 0, err
		}
		branches = append(branches, branch)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, branch := range branches {
		if !dataBranchID.MatchString(branch) {
			return 0, errors.New("Inherited Auth role installation is invalid")
		}
		login := managedAuthLogin(branch)
		var marker string
		var safe, enabled bool
		err = tx.QueryRow(ctx, `SELECT COALESCE(shobj_description(oid,'pg_authid'),''),NOT rolsuper AND NOT rolbypassrls AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolinherit AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member=pg_roles.oid),rolcanlogin FROM pg_roles WHERE rolname=$1`, login).Scan(&marker, &safe, &enabled)
		if err != nil || !safe || marker != "neon-control:managed-auth:"+p.ProjectID+":"+branch {
			return 0, errors.New("Inherited Auth SQL role ownership conflict")
		}
		if enabled {
			if !write {
				return 0, errors.New("Inherited Auth SQL login changed after migration")
			}
			if _, err = tx.Exec(ctx, "ALTER ROLE "+pgQuote(login)+" NOLOGIN"); err != nil {
				return 0, err
			}
		}
	}
	return len(branches), nil
}

// The source is resolved by admission, never accepted as a caller supplied key.
// A historical successful installation and the physical branch marker must agree.
func (s *server) managedAuthFactorSource(ctx context.Context, p managedAuthPayload, generation int64) error {
	var valid bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM branches b JOIN branches parent ON parent.id=b.parent_branch_id AND parent.project_id=b.project_id WHERE b.id=$1 AND b.project_id=$2 AND parent.id=$3) AND EXISTS(SELECT 1 FROM operations WHERE project_id=$2 AND resource_id=$3 AND resource_type='managed_auth' AND action='enable_managed_auth' AND state='succeeded' AND payload->>'branch_id'=$3 AND payload->>'project_id'=$2 AND payload->>'generation'=$4 AND payload->'spec'->>'database'=$5 AND payload->'spec'->>'two_factor_enabled'='true')`, p.BranchID, p.ProjectID, p.FactorSourceBranchID, fmt.Sprint(generation), p.Spec.Database).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("Auth factor source installation is not verified")
	}
	return nil
}

// All changes, including the immutable receipt, share the first installation's
// native SQL transaction. A retry after an unknown commit reads the receipt;
// it must never decrypt or overwrite subsequently changed user factors.
func (s *server) rebindManagedAuthFactorsTx(ctx context.Context, tx pgx.Tx, p managedAuthPayload) (*managedAuthFactorProof, error) {
	if !p.Spec.InheritTwoFactor || !p.Spec.TwoFactorEnabled || p.FactorSourceBranchID == "" || p.FactorOperationID == "" || p.FactorSourceBranchID == p.BranchID || len(s.idempotencyKey) < 32 {
		return nil, errors.New("Explicit verified Auth factor inheritance required")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,922742))`, p.BranchID); err != nil {
		return nil, err
	}
	var marker string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(obj_description(oid,'pg_namespace'),'') FROM pg_namespace WHERE nspname='neon_auth'`).Scan(&marker); err != nil || marker != "neon-control:managed-auth:"+p.ProjectID {
		return nil, errors.New("Auth factor schema ownership conflict")
	}
	var sourceGeneration int64
	if err := tx.QueryRow(ctx, `SELECT generation FROM neon_auth.control_installations WHERE branch_id=$1 FOR UPDATE`, p.FactorSourceBranchID).Scan(&sourceGeneration); err != nil {
		return nil, errors.New("Physical Auth source installation is unavailable")
	}
	if err := s.managedAuthFactorSource(ctx, p, sourceGeneration); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, managedAuthFactorRebindSchema, pgx.QueryExecModeSimpleProtocol); err != nil {
		return nil, err
	}
	var raw []byte
	var mac string
	err := tx.QueryRow(ctx, `SELECT proof,mac FROM neon_auth.control_factor_rebindings WHERE branch_id=$1`, p.BranchID).Scan(&raw, &mac)
	if err == nil {
		var proof managedAuthFactorProof
		if json.Unmarshal(raw, &proof) != nil || !s.validManagedAuthFactorProof(proof, mac, p) || proof.SourceGeneration != sourceGeneration {
			return nil, errors.New("Auth factor migration receipt does not authenticate")
		}
		var installed bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM neon_auth.control_installations WHERE branch_id=$1 AND generation>=$2)`, p.BranchID, proof.TargetGeneration).Scan(&installed); err != nil {
			return nil, err
		}
		if !installed || (proof.TargetGeneration == p.Generation && proof.OperationID != p.FactorOperationID) {
			return nil, errors.New("Auth factor receipt installation mismatch")
		}
		if n, e := inheritedManagedAuthLoginsTx(ctx, tx, p, false); e != nil || n != proof.InheritedLoginsDisabled {
			return nil, errors.New("Auth factor migration login receipt mismatch")
		}
		return &proof, nil
	}
	if !isNoRows(err) {
		return nil, err
	}
	var installed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM neon_auth.control_installations WHERE branch_id=$1)`, p.BranchID).Scan(&installed); err != nil {
		return nil, err
	}
	if installed {
		return nil, errors.New("Existing Auth installation requires its original factor migration receipt")
	}
	// Block concurrent enrollment, factor changes, and user deletion while reading
	// the copied rows. The parent timeline is never opened or modified here.
	if _, err = tx.Exec(ctx, `LOCK TABLE neon_auth."user",neon_auth."twoFactor" IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return nil, err
	}
	var inconsistent bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM neon_auth."user" u LEFT JOIN neon_auth."twoFactor" f ON f."userId"=u.id GROUP BY u.id,u."twoFactorEnabled" HAVING count(f.id)>1 OR (u."twoFactorEnabled" IS TRUE AND (count(f.id)<>1 OR bool_or(f.verified IS FALSE))))`).Scan(&inconsistent); err != nil {
		return nil, err
	}
	if inconsistent {
		return nil, errors.New("Copied Auth factors are inconsistent; migration was not applied")
	}
	sourceKey, err := s.managedAuthTOTPSecret(managedAuthPayload{ProjectID: p.ProjectID, BranchID: p.FactorSourceBranchID})
	if err != nil {
		return nil, err
	}
	targetKey, err := s.managedAuthTOTPSecret(p)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT f.id,f.secret,f."backupCodes",u."twoFactorEnabled" IS TRUE FROM neon_auth."twoFactor" f JOIN neon_auth."user" u ON u.id=f."userId" ORDER BY f.id`)
	if err != nil {
		return nil, err
	}
	type factor struct {
		id, secret, backup string
		enabled            bool
	}
	var factors []factor
	for rows.Next() {
		var f factor
		if err = rows.Scan(&f.id, &f.secret, &f.backup, &f.enabled); err != nil {
			rows.Close()
			return nil, err
		}
		factors = append(factors, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	proof := managedAuthFactorProof{Version: 1, ProjectID: p.ProjectID, BranchID: p.BranchID, Database: p.Spec.Database, SourceBranchID: p.FactorSourceBranchID, SourceGeneration: sourceGeneration, TargetGeneration: p.Generation, OperationID: p.FactorOperationID, RecoveryCodesReset: true}
	proof.InheritedLoginsDisabled, err = inheritedManagedAuthLoginsTx(ctx, tx, p, true)
	if err != nil {
		return nil, err
	}
	sourceHash, targetHash := sha256.New(), sha256.New()
	for _, f := range factors {
		encoded, _ := json.Marshal([]string{f.id, f.secret, f.backup})
		sourceHash.Write(encoded)
		if !f.enabled {
			if _, err = tx.Exec(ctx, `DELETE FROM neon_auth."twoFactor" WHERE id=$1`, f.id); err != nil {
				return nil, err
			}
			proof.PendingRemoved++
			continue
		}
		plain, openErr := openManagedAuthFactor(sourceKey, f.secret)
		if openErr != nil {
			return nil, errManagedAuthFactorCipher
		}
		backup, openErr := openManagedAuthFactor(sourceKey, f.backup)
		var codes []string
		if openErr != nil || json.Unmarshal(backup, &codes) != nil || !strings.HasPrefix(string(backup), "[") {
			clear(plain)
			clear(backup)
			return nil, errManagedAuthFactorCipher
		}
		clear(backup)
		// The configured runtime uses a single string key. Normalize to its
		// legacy hex wire format even when the copied source has a v0 envelope.
		wrapped, sealErr := sealManagedAuthFactor(targetKey, plain, false)
		clear(plain)
		if sealErr != nil {
			return nil, sealErr
		}
		freshBackup, sealErr := sealManagedAuthFactor(targetKey, []byte("[]"), false)
		if sealErr != nil {
			return nil, sealErr
		}
		if _, err = tx.Exec(ctx, `UPDATE neon_auth."twoFactor" SET secret=$2,"backupCodes"=$3 WHERE id=$1`, f.id, wrapped, freshBackup); err != nil {
			return nil, err
		}
		encoded, _ = json.Marshal([]string{f.id, wrapped, freshBackup})
		targetHash.Write(encoded)
		proof.Migrated++
	}
	proof.SourceDigest = hex.EncodeToString(sourceHash.Sum(nil))
	proof.TargetDigest = hex.EncodeToString(targetHash.Sum(nil))
	raw, _ = json.Marshal(proof)
	if _, err = tx.Exec(ctx, `INSERT INTO neon_auth.control_factor_rebindings(branch_id,proof,mac) VALUES($1,$2,$3)`, p.BranchID, raw, s.managedAuthFactorMAC(proof)); err != nil {
		return nil, err
	}
	return &proof, nil
}

func (s *server) publishManagedAuthFactorProof(ctx context.Context, p managedAuthPayload, proof *managedAuthFactorProof) error {
	mac := s.managedAuthFactorMAC(*proof)
	if !s.validManagedAuthFactorProof(*proof, mac, p) {
		return errors.New("Auth factor publication scope conflict")
	}
	raw, _ := json.Marshal(proof)
	_, err := s.db.Exec(ctx, `INSERT INTO managed_auth_factor_results(operation_id,project_id,branch_id,source_branch_id,proof,mac) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(operation_id) DO NOTHING`, proof.OperationID, proof.ProjectID, proof.BranchID, proof.SourceBranchID, raw, mac)
	if err != nil {
		return err
	}
	var stored []byte
	var storedMAC string
	if err = s.db.QueryRow(ctx, `SELECT proof,mac FROM managed_auth_factor_results WHERE operation_id=$1`, proof.OperationID).Scan(&stored, &storedMAC); err != nil {
		return err
	}
	var previous managedAuthFactorProof
	if json.Unmarshal(stored, &previous) != nil || previous != *proof || !hmac.Equal([]byte(storedMAC), []byte(mac)) {
		return errors.New("Auth factor publication conflicts with original receipt")
	}
	return nil
}
