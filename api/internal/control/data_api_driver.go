package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func pgLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

type dataAPIPrerequisiteError struct{ code, message string }

func (e dataAPIPrerequisiteError) Error() string { return e.message }
func dataAPIGuardSchema(branch string) string {
	return "control_da_" + strings.TrimPrefix(branch, "br_")
}

func (s *server) dataAPIConnection(ctx context.Context, p dataAPIPayload) (*pgx.Conn, error) {
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(p.EndpointID)+"-credentials"), nil)
	if err != nil || !owned(secret, p.ProjectID, p.EndpointID) {
		return nil, errors.New("Data API writer credential ownership mismatch")
	}
	password, err := secretText(secret, "probePassword")
	if err != nil {
		return nil, err
	}
	return connectSQL(ctx, s.proxyHost, s.proxyPort, "control_probe", password, p.Spec.Database, selector(p.EndpointID))
}

func (s *server) applyDataAPISQL(ctx context.Context, p dataAPIPayload) error {
	release, err := s.acquireProbeGate(ctx, p.EndpointID, true)
	if err != nil {
		return err
	}
	defer release()
	secret, err := s.dataAPISecret(ctx, p)
	if err != nil {
		return err
	}
	verifier, err := secretText(secret, "verifier")
	if err != nil {
		return err
	}
	conn, err := s.dataAPIConnection(ctx, p)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		return sqlExecutionError{"query", err}
	}
	defer tx.Rollback(ctx)
	if err = applyDataAPISQLTx(ctx, tx, p, verifier); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return sqlExecutionError{"query", err}
	}
	return nil
}

// The same transaction implementation is tested against a disposable real
// PostgreSQL server. Neon acceptance additionally checks the native probe role.
func applyDataAPISQLTx(ctx context.Context, tx pgx.Tx, p dataAPIPayload, verifier string) error {
	var err error
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,922741))`, p.BranchID); err != nil {
		return sqlExecutionError{"query", err}
	}
	// CREATEDB does not grant CREATE on an existing database. The Neon
	// provisioner needs explicit owner delegation to create its private guard
	// schema and grant CONNECT to the restricted service login. This is not a
	// privilege change to either runtime role or an RLS bypass.
	var databaseGranted bool
	if err = tx.QueryRow(ctx, `SELECT has_database_privilege(current_user,current_database(),'CREATE') AND has_database_privilege(current_user,current_database(),'CONNECT WITH GRANT OPTION')`).Scan(&databaseGranted); err != nil {
		return sqlExecutionError{"query", err}
	}
	if !databaseGranted {
		return dataAPIPrerequisiteError{"data_api_database_grant_required", "Database owner must grant CREATE and CONNECT WITH GRANT OPTION to control_probe before enabling Data API"}
	}
	marker := "neon-control:data-api:" + p.ProjectID + ":" + p.BranchID
	login, role := dataAPILogin(p.BranchID), dataAPIRole(p.BranchID)
	for _, name := range []string{login, role} {
		var comment string
		e := tx.QueryRow(ctx, `SELECT COALESCE(shobj_description(oid,'pg_authid'),'') FROM pg_roles WHERE rolname=$1`, name).Scan(&comment)
		if e != nil && !isNoRows(e) {
			return sqlExecutionError{"query", e}
		}
		if e == nil && comment != marker {
			return errors.New("Data API role ownership conflict")
		}
		if isNoRows(e) {
			if _, err = tx.Exec(ctx, "CREATE ROLE "+pgQuote(name)+" NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
				return sqlExecutionError{"query", err}
			}
			if _, err = tx.Exec(ctx, "COMMENT ON ROLE "+pgQuote(name)+" IS "+pgLiteral(marker)); err != nil {
				return sqlExecutionError{"query", err}
			}
		}
	}
	if _, err = tx.Exec(ctx, "ALTER ROLE "+pgQuote(login)+" LOGIN PASSWORD "+pgLiteral(verifier)+" CONNECTION LIMIT 4"); err != nil {
		return sqlExecutionError{"query", err}
	}
	// The pinned pool does not guarantee prompt physical disconnect while no
	// requests arrive. Apply a server-side timeout only to our owned service
	// login. PostgreSQL preserves active queries and idle transactions; neither
	// user roles nor cluster defaults change. Real pool reconnection is a CI gate.
	if _, err = tx.Exec(ctx, "ALTER ROLE "+pgQuote(login)+" SET idle_session_timeout = '5s'"); err != nil {
		return sqlExecutionError{"query", err}
	}
	if _, err = tx.Exec(ctx, "GRANT "+pgQuote(role)+" TO "+pgQuote(login)+" WITH INHERIT FALSE, SET TRUE"); err != nil {
		return sqlExecutionError{"query", err}
	}
	var schemaExists bool
	var unsafe, tables, functions int
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, p.Spec.Schema).Scan(&schemaExists); err != nil {
		return sqlExecutionError{"query", err}
	}
	if !schemaExists {
		return errors.New("application schema does not exist")
	}
	if err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE relkind<>'r' OR NOT relrowsecurity OR NOT relforcerowsecurity OR pg_get_userbyid(relowner)=ANY($2::text[]))
        FROM pg_class WHERE relnamespace=(SELECT oid FROM pg_namespace WHERE nspname=$1) AND relkind IN ('r','v','m','f','p')`, p.Spec.Schema, []string{login, role}).Scan(&tables, &unsafe); err != nil {
		return sqlExecutionError{"query", err}
	}
	if tables == 0 || unsafe != 0 {
		return errors.New("every exposed table must enable and force RLS; views and foreign/partitioned tables require a separate Driver gate")
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE pronamespace=(SELECT oid FROM pg_namespace WHERE nspname=$1)`, p.Spec.Schema).Scan(&functions); err != nil {
		return sqlExecutionError{"query", err}
	}
	if functions != 0 {
		return errors.New("exposed RPC functions require a separate privilege review")
	}
	guard := dataAPIGuardSchema(p.BranchID)
	var comment string
	e := tx.QueryRow(ctx, `SELECT COALESCE(obj_description(oid,'pg_namespace'),'') FROM pg_namespace WHERE nspname=$1`, guard).Scan(&comment)
	if e != nil && !isNoRows(e) {
		return sqlExecutionError{"query", e}
	}
	if e == nil && comment != marker {
		return errors.New("Data API guard schema ownership conflict")
	}
	claimsCheck := fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.check_request() RETURNS void LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog AS $guard$
DECLARE c jsonb := current_setting('request.jwt.claims',true)::jsonb;
BEGIN
 IF c IS NULL OR c->>'iss' IS DISTINCT FROM 'neon-control-data-api' OR c->>'aud' IS DISTINCT FROM %s OR c->>'branch_id' IS DISTINCT FROM %s OR COALESCE(c->>'sub','')='' THEN
   RAISE EXCEPTION 'invalid delegated identity' USING ERRCODE='42501';
 END IF;
 IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication))
    OR EXISTS (SELECT 1 FROM pg_class WHERE relnamespace=(SELECT oid FROM pg_namespace WHERE nspname=%s)
      AND relkind IN ('r','v','m','f','p') AND (relkind<>'r' OR NOT relrowsecurity OR NOT relforcerowsecurity OR pg_get_userbyid(relowner)=current_user)) THEN
   RAISE EXCEPTION 'application RLS configuration changed' USING ERRCODE='42501';
 END IF;
END $guard$`, pgQuote(guard), pgLiteral(p.Spec.Audience), pgLiteral(p.BranchID), pgLiteral(p.Spec.Schema))
	statements := []string{
		"CREATE SCHEMA IF NOT EXISTS " + pgQuote(guard), "COMMENT ON SCHEMA " + pgQuote(guard) + " IS " + pgLiteral(marker),
		"REVOKE ALL ON SCHEMA " + pgQuote(guard) + " FROM PUBLIC", claimsCheck,
		"REVOKE ALL ON FUNCTION " + pgQuote(guard) + ".check_request() FROM PUBLIC",
		"GRANT USAGE ON SCHEMA " + pgQuote(guard) + "," + pgQuote(p.Spec.Schema) + " TO " + pgQuote(role),
		"GRANT EXECUTE ON FUNCTION " + pgQuote(guard) + ".check_request() TO " + pgQuote(role),
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + pgQuote(p.Spec.Schema) + " TO " + pgQuote(role),
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA " + pgQuote(p.Spec.Schema) + " TO " + pgQuote(role),
		"GRANT CONNECT ON DATABASE " + pgQuote(p.Spec.Database) + " TO " + pgQuote(login),
	}
	for _, statement := range statements {
		if _, err = tx.Exec(ctx, statement); err != nil {
			return sqlExecutionError{"query", err}
		}
	}
	var valid bool
	err = tx.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(NOT rolsuper AND NOT rolbypassrls AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolinherit)
        FROM pg_roles WHERE rolname=ANY($1::text[])`, []string{login, role}).Scan(&valid)
	if err != nil {
		return sqlExecutionError{"query", err}
	}
	if !valid {
		return errors.New("Data API role attributes failed verification")
	}
	var unexpected bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_auth_members WHERE member IN (SELECT oid FROM pg_roles WHERE rolname=ANY($1::text[]))
        AND NOT (member=(SELECT oid FROM pg_roles WHERE rolname=$2) AND roleid=(SELECT oid FROM pg_roles WHERE rolname=$3) AND NOT inherit_option AND set_option AND NOT admin_option))`, []string{login, role}, login, role).Scan(&unexpected)
	if err != nil {
		return sqlExecutionError{"query", err}
	}
	if unexpected {
		return errors.New("unexpected Data API role membership")
	}
	return nil
}

func (s *server) publishDataAPIIdentity(ctx context.Context, p dataAPIPayload, enable bool) error {
	verifier := ""
	if enable {
		secret, err := s.dataAPISecret(ctx, p)
		if err != nil {
			return err
		}
		verifier, err = secretText(secret, "verifier")
		if err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 6; attempt++ {
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
		if err != nil {
			return err
		}
		raw, err := secretText(secret, "routes.json")
		if err != nil {
			return err
		}
		var routes map[string]map[string]any
		if err = json.Unmarshal([]byte(raw), &routes); err != nil {
			return err
		}
		route := routes[selector(p.EndpointID)]
		if route == nil || route["project_id"] != p.ProjectID || route["branch_id"] != p.BranchID {
			return errors.New("Data API writer route ownership mismatch")
		}
		roles, ok := route["roles"].(map[string]any)
		if !ok {
			return errors.New("writer route role registry unavailable")
		}
		if enable {
			roles[dataAPILogin(p.BranchID)] = verifier
		} else {
			delete(roles, dataAPILogin(p.BranchID))
		}
		b, _ := json.Marshal(routes)
		secret["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(b)
		_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("secret", routesSecret), secret)
		var ke kubeError
		if errors.As(err, &ke) && ke.Status == 409 {
			continue
		}
		return err
	}
	return errors.New("Data API credential registry conflict")
}

func dataAPIDeployment(p dataAPIPayload, namespace string, enable bool) map[string]any {
	name := dataAPIName(p.BranchID)
	labels := dataAPILabels(p)
	labels["neon-control/data-api"] = p.BranchID
	authVolume := map[string]any{"name": "auth-config", "secret": map[string]any{"secretName": p.SecretRef, "defaultMode": int(0440), "items": []any{map[string]string{"key": "config.json", "path": "config.json"}, map[string]string{"key": "delegation-seed", "path": "delegation-seed"}}}}
	sqlVolume := map[string]any{"name": "sql-config", "secret": map[string]any{"secretName": p.SecretRef, "defaultMode": int(0440), "items": []any{map[string]string{"key": "postgrest.conf", "path": "postgrest.conf"}, map[string]string{"key": "delegation-jwks.json", "path": "delegation-jwks.json"}}}}
	security := map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532, "seccompProfile": map[string]any{"type": "RuntimeDefault"}}
	containerSecurity := map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}}
	mounts := func(name string) []any {
		return []any{map[string]any{"name": name, "mountPath": "/run/data-api", "readOnly": true}, map[string]any{"name": "tmp", "mountPath": "/tmp"}}
	}
	resources := map[string]any{"requests": map[string]string{"cpu": "50m", "memory": "48Mi"}, "limits": map[string]string{"cpu": "500m", "memory": "192Mi"}}
	replicas := 1
	if !enable {
		replicas = 0
	}
	return map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": name, "namespace": namespace, "labels": labels},
		"spec": map[string]any{"replicas": replicas, "strategy": map[string]any{"type": "Recreate"}, "selector": map[string]any{"matchLabels": map[string]string{"neon-control/data-api": p.BranchID}},
			"template": map[string]any{"metadata": map[string]any{"labels": labels, "annotations": map[string]string{"neon-control/generation": fmt.Sprint(p.Generation)}},
				"spec": map[string]any{"automountServiceAccountToken": false, "securityContext": security, "terminationGracePeriodSeconds": 15,
					"containers": []any{
						map[string]any{"name": "auth-entry", "image": env("NEON_DATA_API_GATEWAY_IMAGE", ""), "imagePullPolicy": "IfNotPresent", "securityContext": containerSecurity, "resources": resources, "volumeMounts": mounts("auth-config"),
							"ports":          []any{map[string]any{"name": "data", "containerPort": 9080}, map[string]any{"name": "health", "containerPort": 9081}},
							"env":            []any{map[string]string{"name": "NEON_DATA_API_CONFIG_FILE", "value": "/run/data-api/config.json"}, map[string]string{"name": "NEON_DATA_API_SIGNING_SEED_FILE", "value": "/run/data-api/delegation-seed"}, map[string]string{"name": "NEON_DATA_API_ALLOW_PLAINTEXT_UPSTREAM", "value": "true"}},
							"readinessProbe": map[string]any{"httpGet": map[string]any{"path": "/readyz", "port": "health"}, "periodSeconds": 5}, "livenessProbe": map[string]any{"httpGet": map[string]any{"path": "/healthz", "port": "health"}, "periodSeconds": 15}},
						map[string]any{"name": "postgrest", "image": env("NEON_DATA_API_POSTGREST_IMAGE", ""), "imagePullPolicy": "IfNotPresent", "args": []string{"/run/data-api/postgrest.conf"}, "securityContext": containerSecurity, "resources": resources, "volumeMounts": mounts("sql-config")},
					}, "volumes": []any{authVolume, sqlVolume, map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "16Mi"}}}}}}}
}
func (s *server) reconcileDataAPIWorkload(ctx context.Context, p dataAPIPayload, enable bool) error {
	name := dataAPIName(p.BranchID)
	service := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": name, "labels": dataAPILabels(p)},
		"spec": map[string]any{"type": "ClusterIP", "selector": map[string]string{"neon-control/data-api": p.BranchID}, "ports": []any{map[string]any{"name": "data", "port": 9080, "targetPort": "data"}}}}
	if enable {
		if _, err := s.kube.createOwned(ctx, "service", service, p.ProjectID, p.EndpointID); err != nil {
			return err
		}
	}
	body := dataAPIDeployment(p, s.kube.namespace, enable)
	current, err := s.kube.request(ctx, http.MethodGet, s.kube.path("deployment", name), nil)
	var ke kubeError
	if errors.As(err, &ke) && ke.Status == 404 && enable {
		_, err = s.kube.createOwned(ctx, "deployment", body, p.ProjectID, p.EndpointID)
	} else if err == nil {
		if !ownedDataAPI(current, p) {
			return errors.New("Data API workload ownership mismatch")
		}
		body["metadata"].(map[string]any)["resourceVersion"] = nested(current, "metadata", "resourceVersion")
		_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("deployment", name), body)
	} else if errors.As(err, &ke) && ke.Status == 404 && !enable {
		return nil
	}
	if err != nil {
		return err
	}
	for {
		item, e := s.kube.request(ctx, http.MethodGet, s.kube.path("deployment", name), nil)
		if e != nil {
			return e
		}
		observed := number(nested(item, "status", "observedGeneration")) >= number(nested(item, "metadata", "generation"))
		if observed && ((!enable && number(nested(item, "status", "replicas")) == 0) || (enable && number(nested(item, "status", "availableReplicas")) == 1 && number(nested(item, "status", "updatedReplicas")) == 1)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
func (s *server) verifyDataAPILogin(ctx context.Context, p dataAPIPayload) error {
	secret, err := s.dataAPISecret(ctx, p)
	if err != nil {
		return err
	}
	password, err := secretText(secret, "password")
	if err != nil {
		return err
	}
	var conn *pgx.Conn
	for attempt := 0; attempt < 20; attempt++ {
		conn, err = connectSQL(ctx, s.proxyHost, s.proxyPort, dataAPILogin(p.BranchID), password, p.Spec.Database, selector(p.EndpointID))
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err = conn.Exec(ctx, "SET ROLE "+pgQuote(dataAPIRole(p.BranchID))); err != nil {
		return sqlExecutionError{"query", err}
	}
	var valid bool
	err = conn.QueryRow(ctx, `SELECT NOT rolsuper AND NOT rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&valid)
	if err != nil {
		return sqlExecutionError{"query", err}
	}
	if !valid {
		return errors.New("Data API login privilege verification failed")
	}
	var grants bool
	err = conn.QueryRow(ctx, `SELECT has_schema_privilege(current_user,$1,'USAGE') AND NOT EXISTS (
		SELECT 1 FROM pg_class WHERE relnamespace=(SELECT oid FROM pg_namespace WHERE nspname=$1) AND relkind='r'
		AND NOT (has_table_privilege(current_user,oid,'SELECT') AND has_table_privilege(current_user,oid,'INSERT')
		AND has_table_privilege(current_user,oid,'UPDATE') AND has_table_privilege(current_user,oid,'DELETE')))`, p.Spec.Schema).Scan(&grants)
	if err != nil {
		return sqlExecutionError{"query", err}
	}
	if !grants {
		return errors.New("schema owner must grant application privileges to control_probe WITH GRANT OPTION")
	}
	return nil
}
