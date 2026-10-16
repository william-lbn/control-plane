package control

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

func (s *server) functionSQLIntent(ctx context.Context, op, worker string, p functionInstanceTarget) (functionSQLReference, bool, error) {
	var ref functionSQLReference
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ref, false, errFunctionSQLSecret
	}
	defer tx.Rollback(ctx)
	if err = lockFunctionInstanceLease(ctx, tx, op, worker, p); err != nil {
		return ref, false, err
	}
	ref.Namespace = s.kube.namespace
	ref.Generation = 1
	ref.Name = "fn-" + p.FunctionID[4:] + "-sql-1"
	ref.MACKeyVersion = s.functions.input.KeyVersion
	ref.Scope = functionsql.Scope{FunctionID: p.FunctionID, ProjectID: p.ProjectID, BranchID: p.BranchID, ProvisioningRole: "control_probe"}
	err = tx.QueryRow(ctx, `SELECT endpoint_id,database_name,sql_schema FROM function_definitions WHERE id=$1`, p.FunctionID).Scan(&ref.WriterID, &ref.Scope.Database, &ref.Scope.Schema)
	if err != nil || !ref.valid() {
		return functionSQLReference{}, false, errFunctionSQLSecret
	}
	var persisted bool
	if tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM function_sql_credentials WHERE function_id=$1 AND credential_generation=1)`, p.FunctionID).Scan(&persisted) != nil {
		return functionSQLReference{}, false, errFunctionSQLSecret
	}
	if err = commitFunctionInstance(ctx, tx, op, worker); err != nil {
		return functionSQLReference{}, false, err
	}
	return ref, persisted, nil
}

func (s *server) verifyFunctionIntent(ctx context.Context, op, worker string, p functionInstanceTarget) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return errFunctionInstanceStore
	}
	defer tx.Rollback(ctx)
	if err = lockFunctionInstanceLease(ctx, tx, op, worker, p); err != nil {
		return err
	}
	return commitFunctionInstance(ctx, tx, op, worker)
}

func (s *server) functionSQLPolicy(ref functionSQLReference) functionsql.ConnectionPolicy {
	return functionsql.ConnectionPolicy{Scope: ref.Scope, WriterID: ref.WriterID, Target: s.functions.proxy, Hostname: s.functions.input.ProxyHostname, CA: s.functions.proxyCA, Timeout: 120 * time.Second}
}

func (s *server) reconcileFunctionSQL(ctx context.Context, op, worker string, p functionInstanceTarget) (functionSQLRecord, error) {
	var empty functionSQLRecord
	ref, persisted, err := s.functionSQLIntent(ctx, op, worker, p)
	if err != nil {
		return empty, err
	}
	var record functionSQLRecord
	if persisted {
		ref, err = s.loadFunctionSQLCredential(ctx, op, worker, p)
		if err != nil {
			return empty, err
		}
		if ref.MACKeyVersion != s.functions.input.KeyVersion {
			return empty, errFunctionSQLSecret
		}
		record, err = s.kube.readFunctionSQLSecret(ctx, s.functions.key, ref)
	} else {
		// A previous accepted POST may have lost its result before receipt commit.
		// Find verifies the signed original; reserve sends POST only after its own
		// definite 404. No generated replacement can overwrite an existing Secret.
		record, err = s.kube.findFunctionSQLSecret(ctx, s.functions.key, ref)
		if err != nil {
			if err = s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
				return empty, err
			}
			password := randomToken(32)
			verifier, e := scramVerifier(password)
			if e != nil {
				return empty, errFunctionSQLSecret
			}
			record, err = s.kube.reserveFunctionSQLSecret(ctx, s.functions.key, ref, functionSQLCredential{Password: password, Verifier: verifier})
		}
		if err == nil {
			_, err = s.recordFunctionSQLCredential(ctx, op, worker, p, record.Reference)
		}
	}
	if err != nil {
		return empty, err
	}
	ref = record.Reference
	if err = s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
		return empty, err
	}
	secret, err := s.kube.request(ctx, "GET", s.kube.path("secret", kubeName(ref.WriterID)+"-credentials"), nil)
	if err != nil || !owned(secret, p.ProjectID, ref.WriterID) {
		return empty, errFunctionSQLSecret
	}
	password, err := secretText(secret, "probePassword")
	if err != nil {
		return empty, errFunctionSQLSecret
	}
	release, err := s.acquireProbeGate(ctx, ref.WriterID, true)
	if err != nil {
		return empty, err
	}
	defer release()
	conn, err := functionsql.ConnectProvisioner(ctx, s.functionSQLPolicy(ref), password)
	if err != nil {
		return empty, dataAPIPrerequisiteError{"function_sql_prerequisite_required", "Original Writer TLS/provisioning identity unavailable; inspect the Operation and owner grants"}
	}
	defer conn.Close(context.Background())
	if err = s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
		return empty, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return empty, errFunctionSQLSecret
	}
	defer tx.Rollback(ctx)
	if err = functionsql.ProvisionTx(ctx, tx, ref.Scope, record.Credential.Verifier); err != nil {
		return empty, dataAPIPrerequisiteError{"function_database_grant_required", "Database owner must explicitly authorize CONNECT/schema/table/sequence grants and review PUBLIC TEMP/CREATE permissions before Function deployment"}
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, errFunctionSQLSecret
	}
	if err = s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
		return empty, err
	}
	if err = s.publishBranchServiceIdentity(ctx, p.ProjectID, p.BranchID, ref.WriterID, ref.Scope.Role(), record.Credential.Verifier, true); err != nil {
		return empty, errFunctionSQLSecret
	}
	runtime, err := functionsql.ConnectRuntime(ctx, s.functionSQLPolicy(ref), record.Credential.Password)
	if err != nil {
		return empty, errFunctionSQLSecret
	}
	_ = runtime.Close(context.Background())
	if err = s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
		return empty, err
	}
	return record, nil
}

func functionRandomKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", errFunctionBootstrap
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

func (s *server) functionBootstrapFor(ctx context.Context, op, worker string, p functionInstanceTarget, instance functionInstanceRecord, backing functionDeploymentBacking, sql functionSQLRecord) (functionBootstrapRecord, error) {
	var empty functionBootstrapRecord
	ref := functionBootstrapReference{Namespace: s.kube.namespace, Name: instance.SecretName, MACKeyVersion: s.functions.input.KeyVersion,
		FunctionID: p.FunctionID, WriterID: sql.Reference.WriterID, BundleDigest: backing.Artifact.Digest, Scope: instance.Scope}
	retained, found, err := s.kube.findFunctionBootstrap(ctx, s.functions.key, ref)
	if err != nil {
		return empty, err
	}
	if !found {
		if err = s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
			return empty, err
		}
		identity, e := s.functions.issuer.Issue(instance.Scope, time.Now(), 2*time.Hour)
		if e != nil {
			return empty, errFunctionBootstrap
		}
		managerKey, e := functionRandomKey()
		if e != nil {
			return empty, e
		}
		artifactKey, e := functionRandomKey()
		if e != nil {
			return empty, e
		}
		environment, e := s.kube.readFunctionEnvironment(ctx, s.functions.key, backing.Environment)
		if e != nil {
			return empty, e
		}
		dsn := url.URL{Scheme: "postgresql", User: url.UserPassword(sql.Reference.Scope.Role(), sql.Credential.Password), Host: s.functions.input.ProxyHostname + ":" + strconv.Itoa(int(s.functions.proxy.Port())), Path: "/" + sql.Reference.Scope.Database}
		dsn.RawQuery = url.Values{"sslmode": {"verify-full"}, "sslrootcert": {"/etc/neon-function/sql-ca.crt"}, "options": {"endpoint=" + selector(sql.Reference.WriterID)}}.Encode()
		config := functions.Bootstrap{Scope: instance.Scope, ManagerKey: managerKey, ManagerCertificate: identity.Certificate, ManagerPrivateKey: identity.PrivateKey, ManagerCA: s.functions.guestCA,
			ArtifactURL: s.functions.input.ArtifactOrigin + "/internal/v1/function-artifacts/" + instance.Scope.InstanceID + "/" + p.DeploymentID + "/" + backing.Artifact.Digest, ArtifactKey: artifactKey, ArtifactCA: s.functions.guestCA,
			BundleDigest: backing.Artifact.Digest, DatabaseURL: dsn.String(), WriterEndpointID: sql.Reference.WriterID, SQLCA: s.functions.proxyCA,
			ProxyIP: s.functions.proxy.Addr().String(), ProxyHostname: s.functions.input.ProxyHostname, ProxyPort: s.functions.proxy.Port(), DNSIP: s.functions.dns.String(), Environment: environment}
		retained, err = s.kube.reserveFunctionBootstrap(ctx, s.functions.key, ref, config)
		if err != nil {
			return empty, err
		}
	}
	u, err := url.Parse(retained.Config.DatabaseURL)
	pw, hasPassword := "", false
	if u != nil && u.User != nil {
		pw, hasPassword = u.User.Password()
	}
	if err != nil || u == nil || u.User == nil || !hasPassword || u.User.Username() != sql.Reference.Scope.Role() || u.Path != "/"+sql.Reference.Scope.Database ||
		!hmac.Equal([]byte(pw), []byte(sql.Credential.Password)) || retained.Config.SQLCA != s.functions.proxyCA || retained.Config.ManagerCA != s.functions.guestCA ||
		retained.Config.WriterEndpointID != sql.Reference.WriterID || !strings.HasPrefix(retained.Config.ArtifactURL, s.functions.input.ArtifactOrigin+"/") {
		return empty, errFunctionBootstrap
	}
	if _, err = s.recordFunctionBootstrap(ctx, op, worker, p, retained.Reference); err != nil {
		return empty, err
	}
	return retained, nil
}

func (s *server) functionDeployStep(ctx context.Context, op, worker string, ordinal int, action func() error) error {
	if err := s.setCreateStep(ctx, op, worker, ordinal, "running", ""); err != nil {
		return err
	}
	if err := action(); err != nil {
		_ = s.setCreateStep(ctx, op, worker, ordinal, "failed", "Function reconciliation step failed; inspect Operation identity")
		return err
	}
	return s.setCreateStep(ctx, op, worker, ordinal, "succeeded", "")
}

// This is the actual native execution path: immutable backing -> original SQL
// identity -> candidate/Secret -> actual K8s VM -> authenticated guest status
// -> restricted SQL observation -> atomic metadata publication. A health error
// never releases VM capacity or adopts a replacement UID. Cross-system fencing
// and uninterrupted replacement remain separate production requirements.
func (s *server) reconcileFunctionDeployment(ctx context.Context, op, worker string, p functionInstanceTarget) error {
	if s.functions == nil || s.functions.issuer == nil || s.processRole != roleWorker || !p.valid() {
		return errFunctionsPlatform
	}
	// A prior atomic publication may have committed before its reply was lost.
	// Confirm only that saved commit, not a new guest-health observation. Never
	// allocate another candidate or downgrade its completed deployment on replay.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return errFunctionInstanceStore
	}
	if err = lockFunctionPublicationLease(ctx, tx, op, worker, p); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	var committed bool
	err = tx.QueryRow(ctx, `SELECT f.state='active' AND f.active_deployment_id=$2 AND f.target_deployment_id IS NULL AND d.state='completed'
 AND EXISTS(SELECT 1 FROM function_instances i JOIN function_instance_bootstraps b ON b.instance_id=i.id
 JOIN function_sql_credentials c ON c.function_id=i.function_id WHERE i.function_id=f.id AND i.deployment_id=d.id
 AND i.generation=$3 AND i.state='ready' AND i.vm_uid IS NOT NULL AND i.service_uid IS NOT NULL AND i.boot_id IS NOT NULL)
 FROM function_definitions f JOIN function_deployments d ON d.id=$2 WHERE f.id=$1`, p.FunctionID, p.DeploymentID, p.Generation).Scan(&committed)
	if err != nil {
		_ = tx.Rollback(ctx)
		return errFunctionPublication
	}
	if !committed {
		_, err = tx.Exec(ctx, `UPDATE function_definitions SET state='provisioning',updated_at=now() WHERE id=$1 AND target_deployment_id=$2 AND state='degraded'`, p.FunctionID, p.DeploymentID)
		if err != nil {
			_ = tx.Rollback(ctx)
			return errFunctionInstanceStore
		}
	}
	if err = commitFunctionInstance(ctx, tx, op, worker); err != nil {
		return err
	}
	if committed {
		return s.setCreateStep(ctx, op, worker, 5, "succeeded", "Original atomic publication confirmed; no new readiness claim")
	}
	var backing functionDeploymentBacking
	var sql functionSQLRecord
	var instance functionInstanceRecord
	var bootstrap functionBootstrapRecord
	var plan functionRuntimePlan
	if err := s.functionDeployStep(ctx, op, worker, 0, func() error {
		var err error
		backing, err = s.loadFunctionDeploymentBacking(ctx, op, worker, p)
		if err != nil {
			return err
		}
		if backing.MACKeyVersion != s.functions.input.KeyVersion {
			return errFunctionBackingReceipt
		}
		if _, err = s.storage.readFunctionArtifact(ctx, backing.Artifact); err != nil {
			return err
		}
		_, err = s.kube.readFunctionEnvironment(ctx, s.functions.key, backing.Environment)
		return err
	}); err != nil {
		return err
	}
	if err := s.functionDeployStep(ctx, op, worker, 1, func() error { var err error; sql, err = s.reconcileFunctionSQL(ctx, op, worker, p); return err }); err != nil {
		return err
	}
	if err := s.functionDeployStep(ctx, op, worker, 2, func() error { var err error; instance, err = s.reserveFunctionInstance(ctx, op, worker, p); return err }); err != nil {
		return err
	}
	if err := s.functionDeployStep(ctx, op, worker, 3, func() error {
		var err error
		bootstrap, err = s.functionBootstrapFor(ctx, op, worker, p, instance, backing, sql)
		return err
	}); err != nil {
		return err
	}
	plan = functionRuntimePlan{Target: p, Instance: instance, Bootstrap: bootstrap.Reference, Image: s.functions.input.VMImage, NodeName: s.functions.input.NodeName}
	if err := s.functionDeployStep(ctx, op, worker, 4, func() error {
		if err := s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
			return err
		}
		service, err := s.kube.reserveFunctionRuntimeResource(ctx, s.functions.key, plan, "service")
		if err != nil {
			return err
		}
		instance, err = s.recordFunctionInstanceResources(ctx, op, worker, p, instance.Scope.InstanceID, "", service.UID)
		if err != nil {
			return err
		}
		plan.Instance = instance
		if err = s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
			return err
		}
		vm, err := s.kube.reserveFunctionRuntimeResource(ctx, s.functions.key, plan, "vm")
		if err != nil {
			return err
		}
		instance, err = s.recordFunctionInstanceResources(ctx, op, worker, p, instance.Scope.InstanceID, vm.UID, service.UID)
		if err != nil {
			return err
		}
		plan.Instance = instance
		return nil
	}); err != nil {
		return err
	}
	return s.functionDeployStep(ctx, op, worker, 5, func() error {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var guestDeadline time.Time
		for {
			if err := s.verifyFunctionIntent(ctx, op, worker, p); err != nil {
				return err
			}
			vm, err := s.kube.request(ctx, "GET", s.kube.path("vm", instance.VMName), nil)
			if err != nil {
				return errFunctionRuntimeResource
			}
			if _, err = verifyFunctionRuntimeResource(vm, plan, "vm"); err != nil {
				return err
			}
			if stringVal(nested(vm, "status", "phase")) == "Running" {
				if guestDeadline.IsZero() {
					guestDeadline = time.Now().Add(150 * time.Second)
				}
				if time.Now().After(guestDeadline) {
					return errFunctionGuestBootstrapUnavailable
				}
				service, err := s.kube.request(ctx, "GET", s.kube.path("service", instance.ServiceName), nil)
				if err != nil {
					return errFunctionRuntimeResource
				}
				verified, err := verifyFunctionRuntimeResource(service, plan, "service")
				if err != nil {
					return err
				}
				key, err := base64.StdEncoding.Strict().DecodeString(bootstrap.Config.ManagerKey)
				if err != nil {
					return errFunctionBootstrap
				}
				client, err := functions.NewInstanceClient(instance.Scope, netip.MustParseAddrPort(verified.ClusterIP+":9090"), key, s.functions.guestCA)
				if err != nil {
					return errFunctionBootstrap
				}
				status, err := client.Status(ctx)
				client.Close()
				if err == nil && status.NodeReady && !status.Draining {
					runtime, err := functionsql.ConnectRuntime(ctx, s.functionSQLPolicy(sql.Reference), sql.Credential.Password)
					if err != nil {
						return errFunctionSQLSecret
					}
					_ = runtime.Close(context.Background())
					// Cold SQL authentication can take time. Observe the signed guest
					// again afterwards rather than stamping a stale status as fresh.
					fresh, err := functions.NewInstanceClient(instance.Scope, netip.MustParseAddrPort(verified.ClusterIP+":9090"), key, s.functions.guestCA)
					if err != nil {
						return errFunctionBootstrap
					}
					status, err = fresh.Status(ctx)
					fresh.Close()
					if err != nil {
						return errFunctionPublication
					}
					observation := functionReadyObservation{Status: status, VMUID: *instance.VMUID, ServiceUID: *instance.ServiceUID, BootstrapUID: bootstrap.Reference.UID, ObservedAt: time.Now()}
					_, err = s.publishFunctionInstance(ctx, op, worker, p, observation)
					return err
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	})
}

var errFunctionPreviousInstance = errors.New("Previous Function instance must be explicitly drained before replacement")

var errFunctionGuestBootstrapUnavailable = errors.New("Function guest bootstrap did not become ready within its bounded boot window")
