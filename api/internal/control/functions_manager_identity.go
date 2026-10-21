package control

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

func functionManagerRenewBefore() (time.Duration, error) {
	v := os.Getenv("NEON_FUNCTION_MANAGER_RENEW_BEFORE_SECONDS")
	if v == "" {
		return 45 * time.Minute, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 600 || n > 7140 {
		return 0, errors.New("manager renewal window must be 600 through 7140 seconds")
	}
	return time.Duration(n) * time.Second, nil
}

func (s *server) functionManagerRenewalEnabled() bool {
	return s.processRole == roleWorker && s.functions != nil && s.functions.issuer != nil && os.Getenv("NEON_FUNCTION_MANAGER_RENEWAL_ENABLED") == "true"
}

func (s *server) rememberFunctionManagerIdentity(ctx context.Context, p publishedFunctionRuntime, v functions.ManagerIdentity) error {
	i, b := p.instance, p.bootstrap
	if v.Scope != i.Scope || v.BootID != p.bootID || i.VMUID == nil || i.ServiceUID == nil {
		return errors.New("original manager identity rejected")
	}
	tag, err := s.db.Exec(ctx, `INSERT INTO function_manager_identities(instance_id,function_id,deployment_id,project_id,branch_id,boot_id,vm_uid,service_uid,bootstrap_uid,fingerprint,not_before,not_after,renewals)
 SELECT i.id,f.id,i.deployment_id,f.project_id,f.branch_id,i.boot_id,i.vm_uid,i.service_uid,b.secret_uid,$7,$8,$9,$10
 FROM function_definitions f JOIN function_instances i ON i.function_id=f.id
 JOIN function_instance_bootstraps b ON b.instance_id=i.id JOIN projects p ON p.id=f.project_id
 JOIN branches r ON r.id=f.branch_id JOIN organizations o ON o.id=f.org_id
 WHERE f.id=$1 AND f.state='active' AND f.active_deployment_id=$2 AND f.generation=$3 AND f.version=$4
 AND i.id=$5 AND i.state='ready' AND i.deployment_id=$2 AND i.generation=$3 AND i.boot_id=$6 AND i.vm_uid=$11 AND i.service_uid=$12 AND b.secret_uid=$13
 AND p.state='ready' AND p.deleted_at IS NULL AND r.state='ready' AND r.deleted_at IS NULL AND o.state='active'
 ON CONFLICT(instance_id,fingerprint) DO UPDATE SET last_verified_at=clock_timestamp()
 WHERE function_manager_identities.boot_id=EXCLUDED.boot_id AND function_manager_identities.vm_uid=EXCLUDED.vm_uid
 AND function_manager_identities.service_uid=EXCLUDED.service_uid AND function_manager_identities.bootstrap_uid=EXCLUDED.bootstrap_uid
 AND function_manager_identities.not_before=EXCLUDED.not_before AND function_manager_identities.not_after=EXCLUDED.not_after AND function_manager_identities.renewals=EXCLUDED.renewals`, p.target.FunctionID, p.target.DeploymentID, p.target.Generation, p.version, i.Scope.InstanceID, p.bootID, v.Fingerprint, v.NotBefore, v.NotAfter, v.Renewals, *i.VMUID, *i.ServiceUID, b.UID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("original function intent changed")
	}
	return nil
}

// No SQL wake or customer invocation is used. A lost acknowledgement is
// resolved through a fresh verified handshake to the same admitted boot.
func (s *server) renewFunctionManagerIdentity(ctx context.Context, project, branch, slug string, before time.Duration) error {
	if !s.functionManagerRenewalEnabled() || before < 10*time.Minute || before > 119*time.Minute {
		return errors.New("manager renewal unavailable")
	}
	p, err := s.loadPublishedFunction(ctx, project, branch, slug)
	if err != nil {
		return err
	}
	client, status, err := s.observePublishedFunction(ctx, p)
	if err != nil {
		return err
	}
	defer client.Close()
	capability, err := client.Capabilities(ctx)
	if err != nil {
		return err
	}
	if capability.BootID != p.bootID || !capability.ManagerIdentityRenewal {
		return nil
	}
	current, err := client.ManagerIdentity(ctx)
	if err != nil || current.BootID != p.bootID || status.Draining {
		return errors.New("original manager identity unavailable")
	}
	if err = s.rememberFunctionManagerIdentity(ctx, p, current); err != nil {
		return err
	}
	if current.NotAfter.After(time.Now().Add(before)) {
		return nil
	}
	leaf, err := s.functions.issuer.Issue(p.instance.Scope, time.Now(), 2*time.Hour)
	if err != nil {
		return errors.New("bounded manager leaf unavailable")
	}
	// Recheck the original published intent and Kubernetes UIDs immediately
	// before dispatch. Guest also rejects draining and stale boot/fingerprint.
	check, _, err := s.observePublishedFunction(ctx, p)
	if err != nil {
		return err
	}
	check.Close()
	update := functions.ManagerIdentityUpdate{Scope: p.instance.Scope, BootID: p.bootID, PreviousFingerprint: current.Fingerprint, Certificate: leaf.Certificate, PrivateKey: leaf.PrivateKey}
	writeErr := client.RenewManagerIdentity(ctx, update)
	after, err := client.ManagerIdentity(ctx)
	if err != nil || after.BootID != p.bootID || after.Renewals != current.Renewals+1 || !after.NotAfter.After(current.NotAfter) {
		return errors.New("manager renewal result unknown; retain original boot and observe again")
	}
	// A second leader is not allowed to substitute a different acknowledged
	// certificate. The actual peer must match precisely the leaf we issued.
	if after.Fingerprint != functions.ManagerCertificateFingerprint(leaf.Certificate) {
		return errors.New("manager renewal leaf changed; retain original boot")
	}
	if err = s.rememberFunctionManagerIdentity(ctx, p, after); err != nil {
		return err
	}
	if writeErr != nil {
		s.logger.Info("original manager renewal recovered by authenticated observation", "instance_id", p.instance.Scope.InstanceID)
	}
	return nil
}

func (s *server) runFunctionManagerRenewal(ctx context.Context) {
	if !s.functionManagerRenewalEnabled() {
		return
	}
	before, err := functionManagerRenewBefore()
	if err != nil {
		s.logger.Error("invalid manager certificate renewal window")
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var afterID string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.functionManagerRenewalEnabled() {
				continue
			}
			rows, err := s.db.Query(ctx, `SELECT f.project_id,f.branch_id,f.slug,i.id FROM function_definitions f
 JOIN function_instances i ON i.function_id=f.id AND i.deployment_id=f.active_deployment_id AND i.generation=f.generation
 WHERE f.state='active' AND i.state='ready' AND i.id>$1 ORDER BY i.id LIMIT 4`, afterID)
			if err != nil {
				continue
			}
			var scopes [][3]string
			for rows.Next() {
				var v [3]string
				var id string
				if rows.Scan(&v[0], &v[1], &v[2], &id) == nil {
					scopes = append(scopes, v)
					afterID = id
				}
			}
			rows.Close()
			if len(scopes) == 0 {
				afterID = ""
				continue
			}
			for _, v := range scopes {
				call, cancel := context.WithTimeout(ctx, 30*time.Second)
				err = s.renewFunctionManagerIdentity(call, v[0], v[1], v[2], before)
				cancel()
				if err != nil {
					s.logger.Warn("function manager certificate observation or renewal pending", "project_id", v[0], "branch_id", v[1], "slug", v[2])
				}
			}
		}
	}
}
