package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
	"net/http"
	"sort"
)

func (p catalogRenamePayload) bindingsValid() bool {
	if len(p.ConfigUIDs) == 0 || len(p.ConfigUIDs) > 128 || !functionResourceUID.MatchString(p.RoutesUID) || p.ConfigUIDs[p.EndpointID] == "" {
		return false
	}
	for endpoint, uid := range p.ConfigUIDs {
		if !functionWriterEndpointID.MatchString(endpoint) || !functionResourceUID.MatchString(uid) {
			return false
		}
	}
	return p.Kind != "role" || functionResourceUID.MatchString(p.CredentialUID)
}

func (s *server) catalogRenameEndpoints(ctx context.Context, p catalogRenamePayload) ([]record, error) {
	return s.many(ctx, `SELECT id FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND deleted_at IS NULL ORDER BY id`, p.ProjectID, p.BranchID)
}

func verifyCatalogRenameEndpointSet(ctx context.Context, tx pgx.Tx, p catalogRenamePayload) error {
	rows, err := tx.Query(ctx, `SELECT id FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND deleted_at IS NULL ORDER BY id`, p.ProjectID, p.BranchID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return err
		}
		if _, ok := p.ConfigUIDs[id]; !ok {
			return renameFault(409, "catalog_endpoint_set_changed", "Endpoint set changed before rename admission")
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count != len(p.ConfigUIDs) {
		return renameFault(409, "catalog_endpoint_set_changed", "Endpoint set changed before rename admission")
	}
	return nil
}

func (s *server) catalogRenameVerifier(ctx context.Context, p catalogRenamePayload) (string, error) {
	if p.Kind != "role" {
		return "", nil
	}
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", p.CredentialRef), nil)
	if err != nil || !catalogRenameCredentialOwned(secret, p) || secret["immutable"] != true || stringVal(nested(secret, "metadata", "uid")) != p.CredentialUID {
		return "", errors.New("original immutable managed role credential identity rejected")
	}
	value, err := secretText(secret, "verifier")
	if err != nil || !functionsql.ValidVerifier(value) {
		return "", errors.New("original managed SCRAM verifier unavailable")
	}
	return value, nil
}

func catalogRenameCredentialOwned(secret map[string]any, p catalogRenamePayload) bool {
	if p.NativeIdentity == nil {
		return owned(secret, p.ProjectID, p.EndpointID)
	}
	return p.NativeIdentity.Object.CredentialRef == p.CredentialRef && p.NativeIdentity.Object.CredentialUID == p.CredentialUID && nested(secret, "metadata", "labels", "neon-control/project-id") == p.ProjectID
}

// Bind every external object before admitting the durable Operation. CAS may
// advance resourceVersion; a new Secret at the same name may never be adopted.
func (s *server) captureCatalogRenameBindings(ctx context.Context, p *catalogRenamePayload) error {
	endpoints, err := s.catalogRenameEndpoints(ctx, *p)
	if err != nil {
		return err
	}
	if len(endpoints) == 0 || len(endpoints) > 128 {
		return errors.New("bounded original endpoint set required")
	}
	p.ConfigUIDs = map[string]string{}
	for _, row := range endpoints {
		id := stringVal(row["id"])
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(id)+"-config"), nil)
		if err != nil || !owned(secret, p.ProjectID, id) {
			return errors.New("original compute config unavailable or unowned")
		}
		p.ConfigUIDs[id] = stringVal(nested(secret, "metadata", "uid"))
	}
	routes, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
	if err != nil {
		return err
	}
	p.RoutesUID = stringVal(nested(routes, "metadata", "uid"))
	if p.Kind == "role" {
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", p.CredentialRef), nil)
		if err != nil || nested(secret, "metadata", "labels", "neon-control/project-id") != p.ProjectID || secret["immutable"] != true {
			return errors.New("original managed role credential unavailable")
		}
		p.CredentialUID = stringVal(nested(secret, "metadata", "uid"))
	}
	if !p.bindingsValid() {
		return errors.New("original rename bindings rejected")
	}
	verifier, err := s.catalogRenameVerifier(ctx, *p)
	if err != nil {
		return err
	}
	if err = s.renameCatalogSpecs(ctx, *p, verifier, false); err != nil {
		return err
	}
	return s.renameCatalogRoutes(ctx, *p, verifier, false)
}

func (s *server) renameCatalogSpecs(ctx context.Context, p catalogRenamePayload, verifier string, write bool) error {
	if !p.valid() || !p.bindingsValid() {
		return errors.New("invalid original rename bindings")
	}
	ids := make([]string, 0, len(p.ConfigUIDs))
	for id := range p.ConfigUIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		uid := p.ConfigUIDs[id]
		complete := false
		for attempt := 0; attempt < 6; attempt++ {
			path := s.kube.path("secret", kubeName(id)+"-config")
			secret, err := s.kube.request(ctx, http.MethodGet, path, nil)
			if err != nil || !owned(secret, p.ProjectID, id) || stringVal(nested(secret, "metadata", "uid")) != uid {
				return errors.New("original compute config UID rejected")
			}
			raw, err := secretText(secret, "config.json")
			if err != nil {
				return err
			}
			var config map[string]any
			if json.Unmarshal([]byte(raw), &config) != nil {
				return errors.New("original compute config invalid")
			}
			before, err := json.Marshal(config)
			if err != nil {
				return err
			}
			if err = catalogRenameDelta(config, p, verifier); err != nil {
				return err
			}
			after, err := json.Marshal(config)
			if err != nil {
				return err
			}
			if !write || string(before) == string(after) {
				complete = true
				break
			}
			secret["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(after)
			_, err = s.kube.request(ctx, http.MethodPut, path, secret)
			var conflict kubeError
			if errors.As(err, &conflict) && conflict.Status == 409 {
				continue
			}
			if err != nil {
				return err
			}
			// Re-read and verify the original UID and complete desired transformation.
			current, err := s.kube.request(ctx, http.MethodGet, path, nil)
			if err != nil || !owned(current, p.ProjectID, id) || stringVal(nested(current, "metadata", "uid")) != uid {
				return errors.New("compute config identity changed after CAS")
			}
			raw, err = secretText(current, "config.json")
			if err != nil {
				return err
			}
			config = nil
			if json.Unmarshal([]byte(raw), &config) != nil {
				return errors.New("updated compute config invalid")
			}
			before, _ = json.Marshal(config)
			if err = catalogRenameDelta(config, p, verifier); err != nil {
				return err
			}
			after, _ = json.Marshal(config)
			if string(before) != string(after) {
				continue
			}
			complete = true
			break
		}
		if !complete {
			return errors.New("compute rename CAS conflict budget exhausted")
		}
	}
	return nil
}

func renameCatalogRouteMap(routes map[string]map[string]any, p catalogRenamePayload, verifier string) error {
	for id := range p.ConfigUIDs {
		route := routes[selector(id)]
		if route == nil || route["project_id"] != p.ProjectID || route["branch_id"] != p.BranchID {
			return errors.New("original Proxy route scope rejected")
		}
		if p.Kind != "role" {
			continue
		}
		roles, ok := route["roles"].(map[string]any)
		if !ok {
			return errors.New("original Proxy role inventory unavailable")
		}
		old, hasOld := roles[p.Name]
		target, hasNew := roles[p.NewName]
		if (hasOld && old != verifier) || (hasNew && target != verifier) || (!hasOld && !hasNew) {
			return errors.New("Proxy role differs from original managed credential")
		}
		if hasOld && hasNew {
			return errors.New("Proxy target role already occupied")
		}
		delete(roles, p.Name)
		roles[p.NewName] = verifier
	}
	return nil
}

func (s *server) renameCatalogRoutes(ctx context.Context, p catalogRenamePayload, verifier string, write bool) error {
	for attempt := 0; attempt <= 6; attempt++ {
		path := s.kube.path("secret", routesSecret)
		secret, err := s.kube.request(ctx, http.MethodGet, path, nil)
		if err != nil || stringVal(nested(secret, "metadata", "uid")) != p.RoutesUID {
			return errors.New("original global Proxy routes UID rejected")
		}
		raw, err := secretText(secret, "routes.json")
		if err != nil {
			return err
		}
		var routes map[string]map[string]any
		if json.Unmarshal([]byte(raw), &routes) != nil {
			return errors.New("original Proxy routes invalid")
		}
		before, _ := json.Marshal(routes)
		if err = renameCatalogRouteMap(routes, p, verifier); err != nil {
			return err
		}
		after, _ := json.Marshal(routes)
		if !write || string(before) == string(after) {
			return nil
		}
		if attempt == 6 {
			return errors.New("Proxy rename CAS confirmation budget exhausted")
		}
		secret["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(after)
		_, err = s.kube.request(ctx, http.MethodPut, path, secret)
		var conflict kubeError
		if errors.As(err, &conflict) && conflict.Status == 409 {
			continue
		}
		if err != nil {
			return err
		}
		// A following GET must demonstrate both the original identity and the effect.
	}
	return errors.New("Proxy rename CAS confirmation budget exhausted")
}

func (s *server) reconcileCatalogRename(ctx context.Context, operation, worker string, p catalogRenamePayload) error {
	if !p.valid() || !p.bindingsValid() {
		return errors.New("invalid catalog rename payload")
	}
	recovered, err := s.prepareCatalogRenameResult(ctx, operation, worker, p)
	if err != nil || recovered {
		return err
	}
	if p.NativeIdentity == nil {
		return catalogIdentityRequired()
	}
	if err = s.verifyCatalogCredentialBindings(ctx, p.catalogPayload); err != nil {
		return err
	}
	verifier, err := s.catalogRenameVerifier(ctx, p)
	if err != nil {
		return err
	}
	apply := func(ctx context.Context) error {
		release, err := s.acquireProbeGate(ctx, p.EndpointID, true)
		if err != nil {
			return err
		}
		defer release()
		conn, err := s.catalogConnection(ctx, p.catalogPayload)
		if err != nil {
			return err
		}
		defer conn.Close(context.Background())
		return applyCatalogRenameSQL(ctx, conn, p, verifier)
	}
	steps := []func(context.Context) error{
		func(ctx context.Context) error {
			if err := s.renameCatalogSpecs(ctx, p, verifier, false); err != nil {
				return err
			}
			return s.renameCatalogRoutes(ctx, p, verifier, false)
		},
		apply,
		func(ctx context.Context) error { return s.renameCatalogSpecs(ctx, p, verifier, true) },
		func(ctx context.Context) error { return s.renameCatalogRoutes(ctx, p, verifier, true) },
		func(ctx context.Context) error {
			if err := apply(ctx); err != nil {
				return err
			}
			tx, err := s.db.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if err = assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
				return err
			}
			if err = verifyCatalogRenameEndpointSet(ctx, tx, p); err != nil {
				return err
			}
			if err = commitCatalogRenameResultTx(ctx, tx, operation, worker, p); err != nil {
				return err
			}
			return tx.Commit(ctx)
		},
	}
	for ordinal, step := range steps {
		if err = s.createStep(ctx, operation, worker, ordinal, "catalog_rename", step); err != nil {
			return err
		}
	}
	return nil
}
