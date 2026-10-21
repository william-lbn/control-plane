package control

import (
	"context"
	"errors"
	"net/http"
)

func (s *server) verifyDefaultOwnerTargetCredential(ctx context.Context, p defaultOwnerPayload) error {
	if p.Mode != "managed" {
		return nil
	}
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", p.TargetCredentialRef), nil)
	if err != nil || nested(secret, "metadata", "labels", "neon-control/project-id") != p.ProjectID || stringVal(nested(secret, "metadata", "uid")) != p.TargetCredentialUID || secret["immutable"] != true {
		return errors.New("original managed default Owner credential UID rejected")
	}
	verifier, err := secretText(secret, "verifier")
	if err != nil || catalogMarkerDigest(verifier) != p.TargetVerifierSHA256 {
		return errors.New("original managed default Owner verifier intent rejected")
	}
	return nil
}
func (s *server) observeDefaultOwnerOriginal(ctx context.Context, operation string, p defaultOwnerPayload, requireTarget bool) (bool, error) {
	if err := verifyDefaultOwnerMetadata(ctx, s.db, p, operation, true); err != nil {
		return false, err
	}
	if err := s.verifyDefaultOwnerTargetCredential(ctx, p); err != nil {
		return false, err
	}
	c, err := readDefaultOwnerContext(ctx, s.db, p.ProjectID, p.BranchID)
	if err != nil {
		return false, err
	}
	probe, _, _, err := s.defaultOwnerConnection(ctx, c, "", &p)
	if err != nil {
		return false, err
	}
	defer probe.Close(context.Background())
	return observeDefaultOwnerSQL(ctx, probe, p.Native, requireTarget)
}
func (s *server) defaultOwnerNative(ctx context.Context, operation string, p defaultOwnerPayload, mutate bool) error {
	// A previously completed publication step is not proof that the bound
	// cold objects still exist unchanged when this original Operation retries.
	if mutate {
		if err := s.defaultOwnerSpecs(ctx, p, false, true); err != nil {
			return err
		}
	}
	achieved, err := s.observeDefaultOwnerOriginal(ctx, operation, p, !mutate)
	if err != nil {
		return err
	}
	if achieved || !mutate {
		return nil
	}
	var keyVersion string
	var sealed []byte
	err = s.db.QueryRow(ctx, `SELECT key_version,ciphertext FROM default_owner_credentials WHERE operation_id=$1 AND cleared_at IS NULL`, operation).Scan(&keyVersion, &sealed)
	if err != nil {
		return errors.New("original default Owner credential escrow unavailable")
	}
	password, err := s.openDefaultOwnerPassword(p.credentialScope(operation), keyVersion, sealed)
	if err != nil {
		return err
	}
	c, err := readDefaultOwnerContext(ctx, s.db, p.ProjectID, p.BranchID)
	if err != nil {
		return err
	}
	conn, _, _, err := s.defaultOwnerConnection(ctx, c, password, &p)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return applyDefaultDatabaseOwnerSQL(ctx, conn, p.Native)
}
func (s *server) recoverDefaultOwnerResult(ctx context.Context, operation, worker string, p defaultOwnerPayload) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return err
	}
	done, err := defaultOwnerResultRecorded(ctx, tx, operation, p)
	if err != nil {
		return err
	}
	if !done {
		return errors.New("original default Owner result missing during recovery")
	}
	_, err = tx.Exec(ctx, `UPDATE operation_steps SET state='succeeded',detail=NULL,updated_at=now() WHERE operation_id=$1`, operation)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *server) reconcileDefaultDatabaseOwner(ctx context.Context, operation, worker string, p defaultOwnerPayload) error {
	if !p.valid() {
		return errors.New("invalid original default database Owner payload")
	}
	if done, err := defaultOwnerResultRecorded(ctx, s.db, operation, p); err != nil {
		return err
	} else if done {
		return s.recoverDefaultOwnerResult(ctx, operation, worker, p)
	}
	if err := verifyDefaultOwnerMetadata(ctx, s.db, p, operation, true); err != nil {
		return err
	}
	// The shared session gate prevents normal internal suspension across cold
	// intent publication and native SQL. It is not an external DDL/HA fence.
	release, err := s.acquireProbeGate(ctx, p.EndpointID, true)
	if err != nil {
		return err
	}
	defer release()
	steps := []func(context.Context) error{
		func(ctx context.Context) error {
			if err := s.verifyDefaultOwnerTargetCredential(ctx, p); err != nil {
				return err
			}
			if _, err := s.observeDefaultOwnerOriginal(ctx, operation, p, false); err != nil {
				return err
			}
			return s.defaultOwnerSpecs(ctx, p, false, false)
		},
		func(ctx context.Context) error { return s.defaultOwnerSpecs(ctx, p, true, true) },
		func(ctx context.Context) error { return s.defaultOwnerNative(ctx, operation, p, true) },
		func(ctx context.Context) error {
			if err := s.defaultOwnerNative(ctx, operation, p, false); err != nil {
				return err
			}
			if err := s.defaultOwnerSpecs(ctx, p, false, true); err != nil {
				return err
			}
			tx, err := s.db.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if err = commitDefaultOwnerResultTx(ctx, tx, operation, worker, p); err != nil {
				return err
			}
			return tx.Commit(ctx)
		},
	}
	for ordinal, step := range steps {
		if err = s.createStep(ctx, operation, worker, ordinal, "default_database_owner", step); err != nil {
			return err
		}
	}
	return nil
}
