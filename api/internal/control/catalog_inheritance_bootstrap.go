package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

func classifyCatalogInheritancePrivileges(ctx context.Context, conn *pgx.Conn, source catalogInheritanceSource, objects []catalogInheritanceObject) error {
	if source.SourceBranch == source.Branch {
		return nil
	}
	excluded := map[string]bool{}
	for i := range objects {
		object := &objects[i]
		if object.Kind != "role" {
			continue
		}
		if object.Status == "external_identity" {
			excluded[object.Name] = true
			continue
		}
		var admin bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_auth_members WHERE roleid=$1 AND member=(SELECT oid FROM pg_roles WHERE rolname=current_user) AND admin_option) OR (SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`, object.OID).Scan(&admin); err != nil {
			return err
		}
		if !admin {
			object.Status, object.Reason = "external_identity", "original_service_admin_missing"
			excluded[object.Name] = true
		}
	}
	for i := range objects {
		object := &objects[i]
		if object.Kind != "database" || object.Status == "external_identity" {
			continue
		}
		if excluded[source.Items[i].Owner] {
			object.Status, object.Reason = "external_identity", "source_owner_excluded"
			continue
		}
		var access bool
		if err := conn.QueryRow(ctx, `SELECT pg_has_role(current_user,$1::oid,'USAGE')`, object.OwnerOID).Scan(&access); err != nil {
			return err
		}
		if !access {
			object.Status, object.Reason = "external_identity", "original_owner_access_missing"
		}
	}
	return nil
}

func (s *server) readCatalogInheritanceNative(ctx context.Context, p createPayload, source catalogInheritanceSource, conn *pgx.Conn) ([]catalogInheritanceObject, error) {
	var writerObjects []catalogInheritanceObject
	if p.EndpointType == "read_only" {
		writer, err := s.catalogWriter(ctx, p.ProjectID, p.BranchID)
		if err != nil {
			return nil, err
		}
		primary, err := s.catalogConnection(ctx, catalogPayload{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: writer})
		if err != nil {
			return nil, err
		}
		snapshot, e := readCatalogNativeSnapshot(ctx, primary, source.Scope)
		primary.Close(context.Background())
		if e != nil {
			return nil, e
		}
		writerObjects, err = observeCatalogInheritance(source, snapshot)
		if err != nil {
			return nil, err
		}
	}
	for attempt := 0; attempt < 20; attempt++ {
		snapshot, err := readCatalogNativeSnapshot(ctx, conn, source.Scope)
		if err != nil {
			return nil, err
		}
		objects, err := observeCatalogInheritance(source, snapshot)
		if err != nil {
			return nil, err
		}
		if p.EndpointType != "read_only" {
			return objects, nil
		}
		expected, _ := json.Marshal(writerObjects)
		actual, _ := json.Marshal(objects)
		if string(expected) == string(actual) {
			return objects, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, errors.New("Reader must observe the Writer catalog before publishing inherited identities")
}

func (s *server) verifyCatalogInheritanceCredentials(ctx context.Context, source catalogInheritanceSource) error {
	for _, item := range source.Items {
		if item.Kind != "role" {
			continue
		}
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", item.CredentialRef), nil)
		if err != nil || secret["immutable"] != true || stringVal(nested(secret, "metadata", "labels", "neon-control/project-id")) != source.Project || stringVal(nested(secret, "metadata", "uid")) != item.CredentialUID {
			return errors.New("original inheritance credential UID changed")
		}
	}
	return nil
}
func (s *server) inheritanceRouteIdentity(ctx context.Context, p createPayload) (string, error) {
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
	if err != nil {
		return "", err
	}
	uid := stringVal(nested(secret, "metadata", "uid"))
	if !functionResourceUID.MatchString(uid) {
		return "", errors.New("original inheritance route UID missing")
	}
	raw, err := secretText(secret, "routes.json")
	if err != nil {
		return "", err
	}
	var routes map[string]map[string]any
	if json.Unmarshal([]byte(raw), &routes) != nil {
		return "", errors.New("original inheritance routes rejected")
	}
	route := routes[selector(p.EndpointID)]
	if route == nil || route["project_id"] != p.ProjectID || route["branch_id"] != p.BranchID {
		return "", errors.New("original inheritance route scope changed")
	}
	return uid, nil
}

// Tag only the already-observed original object, in its new physical scope.
// A tag is not a grant, password reset, Owner change or adoption of a new OID.
func claimCatalogInheritanceSQL(ctx context.Context, conn *pgx.Conn, source catalogInheritanceSource, objects []catalogInheritanceObject) error {
	if source.SourceBranch == source.Branch || len(objects) == 0 {
		return nil
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var scope catalogNativeScope
	if err = tx.QueryRow(ctx, `SELECT current_setting('neon.tenant_id',true),current_setting('neon.timeline_id',true)`).Scan(&scope.TenantID, &scope.TimelineID); err != nil || scope != source.Scope {
		return errCatalogInventoryScope
	}
	for _, object := range objects {
		if object.Status == "external_identity" {
			continue
		}
		table, column, description := "pg_database", "datname", "pg_database"
		if object.Kind == "role" {
			table, column, description = "pg_roles", "rolname", "pg_authid"
		}
		var name, marker string
		if err = tx.QueryRow(ctx, "SELECT "+column+",COALESCE(shobj_description(oid,'"+description+"'),'') FROM "+table+" WHERE oid=$1", object.OID).Scan(&name, &marker); err != nil || name != object.Name || marker != "neon-control/branch/"+source.SourceBranch && marker != "neon-control/branch/"+source.Branch {
			return errors.New("inherited tag original OID or name changed")
		}
		if marker != "neon-control/branch/"+source.Branch {
			kind := "DATABASE"
			if object.Kind == "role" {
				kind = "ROLE"
			}
			if _, err = tx.Exec(ctx, "COMMENT ON "+kind+" "+pgQuote(name)+" IS "+pgLiteral("neon-control/branch/"+source.Branch)); err != nil {
				return sqlExecutionError{"inheritance_comment", err}
			}
		}
		var oid uint32
		if err = tx.QueryRow(ctx, "SELECT oid FROM "+table+" WHERE "+column+"=$1", object.Name).Scan(&oid); err != nil || oid != object.OID {
			return errors.New("inherited comment postcondition rejected")
		}
	}
	return tx.Commit(ctx)
}

func applyCatalogInheritanceView(document map[string]any, catalog string, excluded map[string]bool) (map[string]any, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	var copy map[string]any
	if json.Unmarshal(raw, &copy) != nil {
		return nil, errors.New("inherited view config invalid")
	}
	var desired map[string]any
	if json.Unmarshal([]byte(catalog), &desired) != nil {
		return nil, errors.New("inherited desired view invalid")
	}
	cluster, ok := nested(copy, "spec", "cluster").(map[string]any)
	if !ok {
		return nil, errors.New("inherited view cluster missing")
	}
	current, ok := cluster["roles"].([]any)
	if !ok {
		return nil, errors.New("inherited view platform roles missing")
	}
	roles := []any{}
	for _, entry := range current {
		item, ok := entry.(map[string]any)
		if !ok {
			return nil, errors.New("inherited view role invalid")
		}
		if item["name"] == "cloud_admin" || item["name"] == "control_probe" {
			roles = append(roles, item)
		}
	}
	extra, ok := desired["roles"].([]any)
	if !ok {
		return nil, errors.New("inherited desired roles invalid")
	}
	cluster["roles"] = append(roles, extra...)
	cluster["databases"] = desired["databases"]
	copy["spec"].(map[string]any)["delta_operations"] = desired["delta_operations"]
	return catalogInheritanceConfig(copy, false, excluded)
}

func (s *server) publishCatalogInheritance(ctx context.Context, operation, worker string, p createPayload, v catalogInheritanceObservation, catalog string) error {
	excluded := map[string]bool{}
	observedNames := map[string]bool{}
	for _, object := range v.Objects {
		observedNames[object.Kind+"/"+object.Name] = true
		if object.Status == "external_identity" {
			excluded[object.Kind+"/"+object.Name] = true
		}
	}
	var projection map[string]any
	if json.Unmarshal([]byte(catalog), &projection) != nil {
		return errors.New("original inherited projection invalid")
	}
	for _, kind := range []string{"role", "database"} {
		entries, ok := projection[kind+"s"].([]any)
		if !ok {
			return errors.New("original inherited projection inventory missing")
		}
		for _, entry := range entries {
			item, ok := entry.(map[string]any)
			if !ok {
				return errors.New("original inherited projection entry rejected")
			}
			name := stringVal(item["name"])
			if !observedNames[kind+"/"+name] && !(kind == "database" && name == "postgres") {
				return errors.New("inherited view contains an unobserved customer object")
			}
		}
	}
	roles := map[string]any{}
	configDone := false
	for attempt := 0; attempt < 6; attempt++ {
		secret, document, current, err := s.kube.readComputeCatalogBootstrap(ctx, p)
		if err != nil {
			return err
		}
		if current.ConfigUID != v.ConfigUID || current.CredentialUID != v.CredentialUID {
			return errors.New("inheritance fixed config or credential UID changed")
		}
		desired, err := applyCatalogInheritanceView(document, catalog, excluded)
		if err != nil {
			return err
		}
		before, _ := json.Marshal(document)
		after, _ := json.Marshal(desired)
		if string(before) == string(after) {
			for _, entry := range nested(desired, "spec", "cluster", "roles").([]any) {
				role := entry.(map[string]any)
				roles[stringVal(role["name"])] = role["encrypted_password"]
			}
			configDone = true
			break
		}
		secret["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(after)
		if _, err = s.kube.request(ctx, http.MethodPut, s.kube.path("secret", kubeName(p.EndpointID)+"-config"), secret); err != nil && !kubeStatusIs(err, http.StatusConflict) {
			return err
		}
	}
	if !configDone {
		return errors.New("original inheritance config publication requires fixed UID readback")
	}
	routeDone := false
	for attempt := 0; attempt < 6; attempt++ {
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
		if err != nil {
			return err
		}
		if stringVal(nested(secret, "metadata", "uid")) != v.RoutesUID {
			return errors.New("original inheritance routes UID changed")
		}
		raw, err := secretText(secret, "routes.json")
		if err != nil {
			return err
		}
		var routes map[string]map[string]any
		if json.Unmarshal([]byte(raw), &routes) != nil {
			return errors.New("inheritance route inventory invalid")
		}
		route := routes[selector(p.EndpointID)]
		if route == nil || route["project_id"] != p.ProjectID || route["branch_id"] != p.BranchID {
			return errors.New("inheritance original route scope rejected")
		}
		existing, _ := json.Marshal(route["roles"])
		desired, _ := json.Marshal(roles)
		if string(existing) == string(desired) {
			routeDone = true
			break
		}
		route["roles"] = roles
		encoded, _ := json.Marshal(routes)
		secret["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(encoded)
		if _, err = s.kube.request(ctx, http.MethodPut, s.kube.path("secret", routesSecret), secret); err != nil && !kubeStatusIs(err, http.StatusConflict) {
			return err
		}
	}
	if !routeDone {
		return errors.New("original inheritance route publication requires fixed UID readback")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return err
	}
	if v.Operation != operation {
		return errors.New("original inheritance publication operation changed")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO catalog_inheritance_publications(endpoint_id) VALUES($1) ON CONFLICT DO NOTHING`, p.EndpointID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) SELECT actor_id,'publish_catalog_inheritance','branch_catalog',resource_id,request_id,'succeeded' FROM operations WHERE id=$1`, operation); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *server) finishCatalogInheritance(ctx context.Context, operation, worker string, p createPayload) error {
	source, err := readCatalogInheritanceSource(ctx, s.db, operation, p)
	if err != nil {
		return err
	}
	prior, published, err := readCatalogInheritanceObservation(ctx, s.db, operation, p)
	if err != nil && !isNoRows(err) {
		return err
	}
	if published {
		// Later role generations are legitimate. Do not republish this old view.
		_, _, current, e := s.kube.readComputeCatalogBootstrap(ctx, p)
		if e != nil {
			return e
		}
		uid, e := s.inheritanceRouteIdentity(ctx, p)
		if e != nil {
			return e
		}
		if current.ConfigUID != prior.ConfigUID || current.CredentialUID != prior.CredentialUID || uid != prior.RoutesUID {
			return errors.New("original inherited publication backing changed")
		}
		return nil
	}
	if err = s.verifyCatalogInheritanceCredentials(ctx, source); err != nil {
		return err
	}
	_, _, backing, err := s.kube.readComputeCatalogBootstrap(ctx, p)
	if err != nil {
		return err
	}
	routesUID, err := s.inheritanceRouteIdentity(ctx, p)
	if err != nil {
		return err
	}
	conn, err := s.catalogConnection(ctx, catalogPayload{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID})
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	objects, err := s.readCatalogInheritanceNative(ctx, p, source, conn)
	if err != nil {
		return err
	}
	if p.EndpointType != "read_only" {
		if err = classifyCatalogInheritancePrivileges(ctx, conn, source, objects); err != nil {
			return err
		}
	}
	v := catalogInheritanceObservation{Operation: operation, SourceSHA256: catalogInheritanceSourceDigest(source), Project: p.ProjectID, Branch: p.BranchID, Endpoint: p.EndpointID, Scope: source.Scope, ConfigUID: backing.ConfigUID, CredentialUID: backing.CredentialUID, RoutesUID: routesUID, Objects: objects}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = storeCatalogInheritanceObservationTx(ctx, tx, operation, worker, p, source, v); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if p.EndpointType != "read_only" {
		if err = claimCatalogInheritanceSQL(ctx, conn, source, objects); err != nil {
			return err
		}
		owner, e := s.defaultOwnerCatalogProjection(ctx, p.ProjectID, p.BranchID)
		if e != nil {
			return e
		}
		if e = s.verifyInheritedDefaultOwner(ctx, p, owner); e != nil {
			return e
		}
	}
	if err = s.sealComputeCatalogBootstrap(ctx, operation, worker, p, false); err != nil {
		return err
	}
	catalog, err := s.projectComputeCatalog(ctx, p.ProjectID, p.BranchID)
	if err != nil {
		return err
	}
	return s.publishCatalogInheritance(ctx, operation, worker, p, v, catalog)
}
