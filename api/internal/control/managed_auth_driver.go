package control

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/url"
	"time"
)

//go:embed assets/auth-schema.sql
var managedAuthSchema string

func (s *server) reserveManagedAuth(ctx context.Context, p managedAuthPayload) error {
	password := randomToken(32)
	verifier, err := scramVerifier(password)
	if err != nil {
		return err
	}
	dsn := &url.URL{Scheme: "postgresql", User: url.UserPassword(managedAuthLogin(p.BranchID), password), Host: s.proxyHost + ":" + s.proxyPort, Path: "/" + p.Spec.Database}
	q := dsn.Query()
	q.Set("sslmode", "verify-full")
	q.Set("options", "endpoint="+selector(p.EndpointID))
	dsn.RawQuery = q.Encode()
	config, _ := json.Marshal(record{"version": 1, "branchID": p.BranchID, "baseURL": managedAuthBase(p.BranchID), "databaseURL": dsn.String(), "databaseTLS": record{"caFile": "/run/auth-ca/ca.crt", "serverName": env("NEON_AUTH_PG_SERVER_NAME", "")}, "secret": randomToken(48), "trustedOrigins": p.Spec.AllowedOrigins, "allowLabHTTP": true})
	_, err = s.kube.createOwned(ctx, "secret", record{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true, "metadata": record{"name": p.SecretRef, "labels": managedAuthLabels(p)}, "stringData": map[string]string{"config.json": string(config), "password": password, "verifier": verifier}}, p.ProjectID, p.EndpointID)
	return err
}
func (s *server) managedAuthSecret(ctx context.Context, p managedAuthPayload) (map[string]any, error) {
	v, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", p.SecretRef), nil)
	if err != nil || !ownedManagedAuth(v, p) {
		return nil, errors.New("Managed Auth Secret ownership mismatch")
	}
	return v, nil
}

func applyManagedAuthSQLTx(ctx context.Context, tx pgx.Tx, p managedAuthPayload, verifier string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,922742))`, p.BranchID); err != nil {
		return err
	}
	var granted bool
	if err := tx.QueryRow(ctx, `SELECT has_database_privilege(current_user,current_database(),'CREATE') AND has_database_privilege(current_user,current_database(),'CONNECT WITH GRANT OPTION')`).Scan(&granted); err != nil {
		return err
	}
	if !granted {
		return dataAPIPrerequisiteError{"auth_database_grant_required", "Database owner must grant CREATE and CONNECT WITH GRANT OPTION to control_probe before enabling Auth"}
	}
	marker := "neon-control:managed-auth:" + p.ProjectID
	var comment string
	err := tx.QueryRow(ctx, `SELECT COALESCE(obj_description(oid,'pg_namespace'),'') FROM pg_namespace WHERE nspname='neon_auth'`).Scan(&comment)
	if err != nil && !isNoRows(err) {
		return err
	}
	if err == nil && comment != marker {
		return errors.New("neon_auth schema ownership conflict")
	}
	if isNoRows(err) {
		if _, err = tx.Exec(ctx, `CREATE SCHEMA neon_auth; REVOKE ALL ON SCHEMA neon_auth FROM PUBLIC; SET LOCAL search_path=neon_auth,pg_catalog;`+managedAuthSchema, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "COMMENT ON SCHEMA neon_auth IS "+pgLiteral(marker)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `CREATE TABLE neon_auth.control_installations(branch_id text PRIMARY KEY,generation bigint NOT NULL,installed_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
	}
	// An inherited installation may keep users/accounts, but never inherited
	// sessions or signing keys. The marker makes interruption/retry idempotent.
	var generation int64
	err = tx.QueryRow(ctx, `SELECT generation FROM neon_auth.control_installations WHERE branch_id=$1 FOR UPDATE`, p.BranchID).Scan(&generation)
	if err != nil && !isNoRows(err) {
		return err
	}
	if generation > p.Generation {
		return errors.New("Managed Auth SQL generation advanced")
	}
	if generation < p.Generation {
		if _, err = tx.Exec(ctx, `DELETE FROM neon_auth.session; DELETE FROM neon_auth.jwks; DELETE FROM neon_auth."rateLimit";`, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO neon_auth.control_installations(branch_id,generation) VALUES($1,$2) ON CONFLICT(branch_id) DO UPDATE SET generation=$2,installed_at=now()`, p.BranchID, p.Generation); err != nil {
			return err
		}
	}
	login := managedAuthLogin(p.BranchID)
	comment = ""
	err = tx.QueryRow(ctx, `SELECT COALESCE(shobj_description(oid,'pg_authid'),'') FROM pg_roles WHERE rolname=$1`, login).Scan(&comment)
	if err != nil && !isNoRows(err) {
		return err
	}
	roleMarker := marker + ":" + p.BranchID
	if err == nil && comment != roleMarker {
		return errors.New("Managed Auth role ownership conflict")
	}
	if isNoRows(err) {
		if _, err = tx.Exec(ctx, "CREATE ROLE "+pgQuote(login)+" NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "COMMENT ON ROLE "+pgQuote(login)+" IS "+pgLiteral(roleMarker)); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		"ALTER ROLE " + pgQuote(login) + " LOGIN PASSWORD " + pgLiteral(verifier) + " CONNECTION LIMIT 4",
		"ALTER ROLE " + pgQuote(login) + " SET idle_session_timeout='5s'",
		"ALTER ROLE " + pgQuote(login) + " SET search_path=neon_auth,pg_catalog",
		"GRANT CONNECT ON DATABASE " + pgQuote(p.Spec.Database) + " TO " + pgQuote(login),
		"GRANT USAGE ON SCHEMA neon_auth TO " + pgQuote(login),
	} {
		if _, err = tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	for _, table := range []string{"user", "session", "account", "verification", "jwks", "rateLimit"} {
		if _, err = tx.Exec(ctx, "GRANT SELECT,INSERT,UPDATE,DELETE ON TABLE neon_auth."+pgQuote(table)+" TO "+pgQuote(login)); err != nil {
			return err
		}
	}
	var safe bool
	err = tx.QueryRow(ctx, `SELECT NOT rolsuper AND NOT rolbypassrls AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolinherit AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member=pg_roles.oid) FROM pg_roles WHERE rolname=$1`, login).Scan(&safe)
	if err != nil {
		return err
	}
	if !safe {
		return errors.New("Managed Auth runtime role has unexpected privileges")
	}
	return nil
}
func (s *server) applyManagedAuthSQL(ctx context.Context, p managedAuthPayload) error {
	release, err := s.acquireProbeGate(ctx, p.EndpointID, true)
	if err != nil {
		return err
	}
	defer release()
	secret, err := s.managedAuthSecret(ctx, p)
	if err != nil {
		return err
	}
	verifier, err := secretText(secret, "verifier")
	if err != nil {
		return err
	}
	conn, err := s.dataAPIConnection(ctx, dataAPIPayload{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID, Spec: DataAPISpec{Database: p.Spec.Database}})
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = applyManagedAuthSQLTx(ctx, tx, p, verifier); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func managedAuthDeployment(p managedAuthPayload, namespace string, enable bool) map[string]any {
	replicas := 1
	if !enable {
		replicas = 0
	}
	labels := managedAuthLabels(p)
	return record{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": record{"name": managedAuthName(p.BranchID), "namespace": namespace, "labels": labels}, "spec": record{
		"replicas": replicas, "strategy": record{"type": "Recreate"}, "selector": record{"matchLabels": map[string]string{"neon-control/managed-auth": p.BranchID}}, "template": record{"metadata": record{"labels": labels, "annotations": map[string]string{"neon-control/generation": fmt.Sprint(p.Generation)}}, "spec": record{
			"automountServiceAccountToken": false, "terminationGracePeriodSeconds": 20, "securityContext": record{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532, "seccompProfile": record{"type": "RuntimeDefault"}},
			"containers": []any{record{"name": "managed-auth", "image": env("NEON_AUTH_RUNTIME_IMAGE", ""), "imagePullPolicy": "IfNotPresent", "ports": []any{record{"name": "auth", "containerPort": 9082}}, "env": []any{record{"name": "NEON_AUTH_CONFIG_FILE", "value": "/run/auth/config.json"}}, "securityContext": record{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": record{"drop": []string{"ALL"}}}, "resources": record{"requests": map[string]string{"cpu": "50m", "memory": "64Mi"}, "limits": map[string]string{"cpu": "500m", "memory": "256Mi"}}, "volumeMounts": []any{record{"name": "config", "mountPath": "/run/auth", "readOnly": true}, record{"name": "ca", "mountPath": "/run/auth-ca", "readOnly": true}, record{"name": "tmp", "mountPath": "/tmp"}}, "readinessProbe": record{"httpGet": record{"path": "/readyz", "port": "auth"}, "periodSeconds": 5}, "livenessProbe": record{"httpGet": record{"path": "/livez", "port": "auth"}, "periodSeconds": 10}}},
			"volumes":    []any{record{"name": "config", "secret": record{"secretName": p.SecretRef, "defaultMode": 0440, "items": []any{record{"key": "config.json", "path": "config.json"}}}}, record{"name": "ca", "secret": record{"secretName": env("NEON_AUTH_PG_CA_SECRET", ""), "defaultMode": 0440, "items": []any{record{"key": env("NEON_AUTH_PG_CA_KEY", "ca.crt"), "path": "ca.crt"}}}}, record{"name": "tmp", "emptyDir": record{"sizeLimit": "16Mi"}}},
		}}}}
}
func (s *server) reconcileManagedAuthWorkload(ctx context.Context, p managedAuthPayload, enable bool) error {
	name := managedAuthName(p.BranchID)
	if enable {
		if _, err := s.kube.createOwned(ctx, "service", record{"apiVersion": "v1", "kind": "Service", "metadata": record{"name": name, "labels": managedAuthLabels(p)}, "spec": record{"selector": map[string]string{"neon-control/managed-auth": p.BranchID}, "ports": []any{record{"name": "auth", "port": 9082, "targetPort": "auth"}}}}, p.ProjectID, p.EndpointID); err != nil {
			return err
		}
	}
	body := managedAuthDeployment(p, s.kube.namespace, enable)
	current, err := s.kube.request(ctx, http.MethodGet, s.kube.path("deployment", name), nil)
	if kubeStatusIs(err, 404) {
		if !enable {
			return nil
		}
		_, err = s.kube.createOwned(ctx, "deployment", body, p.ProjectID, p.EndpointID)
	} else if err == nil {
		if !ownedManagedAuth(current, p) {
			return errors.New("Managed Auth workload ownership mismatch")
		}
		body["metadata"].(map[string]any)["resourceVersion"] = nested(current, "metadata", "resourceVersion")
		_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("deployment", name), body)
	}
	if err != nil {
		return err
	}
	for {
		v, e := s.kube.request(ctx, http.MethodGet, s.kube.path("deployment", name), nil)
		if e != nil {
			return e
		}
		if number(nested(v, "status", "observedGeneration")) >= number(nested(v, "metadata", "generation")) && ((!enable && number(nested(v, "status", "replicas")) == 0) || (enable && number(nested(v, "status", "availableReplicas")) == 1 && number(nested(v, "status", "updatedReplicas")) == 1)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
func (s *server) reconcileManagedAuth(ctx context.Context, id, worker, action string, p managedAuthPayload) error {
	if !managedAuthEnabled() || validateManagedAuth(p) != nil {
		return errors.New("Managed Auth Driver unavailable")
	}
	var generation int64
	if err := s.db.QueryRow(ctx, `SELECT generation FROM managed_auth_instances WHERE branch_id=$1 AND project_id=$2 AND endpoint_id=$3 AND secret_ref=$4`, p.BranchID, p.ProjectID, p.EndpointID, p.SecretRef).Scan(&generation); err != nil || generation != p.Generation {
		return errors.New("Managed Auth intent generation mismatch")
	}
	enable := action == "enable_managed_auth"
	steps := []func(context.Context) error{
		func(ctx context.Context) error {
			if !enable {
				return nil
			}
			return s.reserveManagedAuth(ctx, p)
		},
		func(ctx context.Context) error {
			if !enable {
				return nil
			}
			return s.applyManagedAuthSQL(ctx, p)
		},
		func(ctx context.Context) error {
			verifier := ""
			if enable {
				secret, err := s.managedAuthSecret(ctx, p)
				if err != nil {
					return err
				}
				verifier, err = secretText(secret, "verifier")
				if err != nil {
					return err
				}
			}
			return s.publishBranchServiceIdentity(ctx, p.ProjectID, p.BranchID, p.EndpointID, managedAuthLogin(p.BranchID), verifier, enable)
		},
		func(ctx context.Context) error { return s.reconcileManagedAuthWorkload(ctx, p, enable) },
		func(ctx context.Context) error {
			tx, err := s.db.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
				return err
			}
			state := "active"
			if !enable {
				state = "disabled"
			}
			tag, err := tx.Exec(ctx, `UPDATE managed_auth_instances SET state=$5,updated_at=now() WHERE branch_id=$1 AND project_id=$2 AND generation=$3 AND secret_ref=$4`, p.BranchID, p.ProjectID, p.Generation, p.SecretRef, state)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errLeaseLost
			}
			_, err = tx.Exec(ctx, `INSERT INTO branch_service_instances(branch_id,service_kind,desired_state,observed_state,driver_version,public_endpoint,version,last_observed_at) VALUES($1,'auth',$2,$2,'better-auth-1.7.7',$3,$4,now()) ON CONFLICT(branch_id,service_kind) DO UPDATE SET desired_state=$2,observed_state=$2,driver_version='better-auth-1.7.7',public_endpoint=$3,version=$4,last_observed_at=now()`, p.BranchID, state, managedAuthBase(p.BranchID), p.Generation)
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		},
	}
	for i, step := range steps {
		if err := s.createStep(ctx, id, worker, i, "managed_auth_reconcile", step); err != nil {
			return err
		}
	}
	return nil
}

// Explicit application-user inspection cold wakes SQL. Neither metadata reads
// nor monitoring query the account/password/session/signing-key tables.
func (s *server) managedAuthUsers(w http.ResponseWriter, r *http.Request) {
	var p managedAuthPayload
	p.ProjectID = r.PathValue("project")
	p.BranchID = r.PathValue("branch")
	var raw []byte
	if err := s.db.QueryRow(r.Context(), `SELECT endpoint_id,spec FROM managed_auth_instances WHERE branch_id=$1 AND project_id=$2 AND state='active'`, p.BranchID, p.ProjectID).Scan(&p.EndpointID, &raw); err != nil {
		fail(w, r, 404, "not_found", "Active Auth instance required")
		return
	}
	if json.Unmarshal(raw, &p.Spec) != nil {
		fail(w, r, 503, "metadata_unavailable", "Invalid Auth intent")
		return
	}
	release, err := s.acquireProbeGate(r.Context(), p.EndpointID, true)
	if err != nil {
		fail(w, r, 503, "auth_unavailable", "Could not admit Auth read")
		return
	}
	defer release()
	conn, err := s.dataAPIConnection(r.Context(), dataAPIPayload{ProjectID: p.ProjectID, EndpointID: p.EndpointID, Spec: DataAPISpec{Database: p.Spec.Database}})
	if err != nil {
		s.sqlFailureResponse(w, r, err)
		return
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(r.Context(), `SELECT id,name,email,"emailVerified" AS email_verified,"createdAt" AS created_at FROM neon_auth."user" ORDER BY "createdAt" DESC,id LIMIT 100`)
	if err != nil {
		s.sqlFailureResponse(w, r, sqlExecutionError{"query", err})
		return
	}
	items, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		fail(w, r, 503, "auth_unavailable", "Could not read Auth users")
		return
	}
	if items == nil {
		items = []map[string]any{}
	}
	jsonResponse(w, 200, record{"items": items, "limit": 100, "branch_id": p.BranchID})
}

// Parent users are copied by Neon timelines. This durable companion Operation
// provisions a distinct branch URL/Secret/key domain after the child writer is
// ready. It is queued atomically with creation's final state; a worker restart
// cannot omit it or enqueue a second instance. Branches without a writer wait
// until their first writer is created. Readers never get an Auth writer login.
func (s *server) queueInheritedManagedAuthTx(ctx context.Context, tx pgx.Tx, creationID string, p createPayload) error {
	if !managedAuthEnabled() {
		return nil
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT a.spec FROM branches b JOIN managed_auth_instances a ON a.branch_id=b.parent_branch_id AND a.state='active' WHERE b.id=$1 AND NOT EXISTS(SELECT 1 FROM managed_auth_instances WHERE branch_id=b.id)`, p.BranchID).Scan(&raw)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	auth := managedAuthPayload{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID, Generation: 1, SecretRef: managedAuthName(p.BranchID) + "-g1"}
	if err = json.Unmarshal(raw, &auth.Spec); err != nil {
		return err
	}
	spec, _ := json.Marshal(auth.Spec)
	payload, _ := json.Marshal(auth)
	if _, err = tx.Exec(ctx, `INSERT INTO managed_auth_instances(branch_id,project_id,endpoint_id,generation,state,spec,secret_ref) VALUES($1,$2,$3,1,'provisioning',$4,$5)`, p.BranchID, p.ProjectID, p.EndpointID, spec, auth.SecretRef); err != nil {
		return err
	}
	id := newID("op_")
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) SELECT $1,project_id,'managed_auth',$2,'enable_managed_auth','queued',actor_id,request_id,$3 FROM operations WHERE id=$4`, id, p.BranchID, payload, creationID); err != nil {
		return err
	}
	for i, step := range []string{"reserve_service_credentials", "apply_restricted_database_identity", "publish_service_proxy_identity", "reconcile_service_workload", "verify_and_commit_service"} {
		if _, err = tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'queued')`, id, i, step); err != nil {
			return err
		}
	}
	return nil
}
