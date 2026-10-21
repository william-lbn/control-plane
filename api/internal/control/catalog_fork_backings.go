package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

type catalogForkBacking struct {
	EndpointID, BranchID, OperationID, ProjectID, ConfigUID, RoutesUID, State string
	Digests                                                                   map[string]string
}

func validCatalogForkDigests(digests map[string]string) bool {
	if len(digests) == 0 || len(digests) > 500 {
		return false
	}
	for name, digest := range digests {
		if !validPGIdentifier(name) || protectedPGRole(name) || len(digest) != 64 {
			return false
		}
	}
	return true
}

// Remove only the old platform credential intent. No SQL DROP/ALTER or native
// configure call is made; the copied replacement role remains a SQL object.
func filterCatalogForkConfig(config map[string]any, digests map[string]string) error {
	if !validCatalogForkDigests(digests) {
		return errors.New("bounded excluded role credential proof required")
	}
	spec, ok := config["spec"].(map[string]any)
	if !ok {
		return errors.New("original fork compute spec unavailable")
	}
	cluster, ok := spec["cluster"].(map[string]any)
	if !ok {
		return errors.New("original fork compute cluster unavailable")
	}
	roles, ok := cluster["roles"].([]any)
	if !ok {
		return errors.New("original fork compute roles unavailable")
	}
	databases, ok := cluster["databases"].([]any)
	if !ok {
		return errors.New("original fork compute databases unavailable")
	}
	for _, item := range databases {
		row, ok := item.(map[string]any)
		if !ok {
			return errors.New("original fork database entry rejected")
		}
		if _, excluded := digests[stringVal(row["owner"])]; excluded {
			return errors.New("excluded role still owns a configured database")
		}
	}
	kept := []any{}
	seen := map[string]bool{}
	for _, item := range roles {
		row, ok := item.(map[string]any)
		if !ok {
			return errors.New("original fork role entry rejected")
		}
		name := stringVal(row["name"])
		digest, excluded := digests[name]
		if !excluded {
			kept = append(kept, row)
			continue
		}
		if seen[name] || catalogMarkerDigest(stringVal(row["encrypted_password"])) != digest {
			return errors.New("original excluded credential configuration changed")
		}
		seen[name] = true
		for key := range row {
			if key != "name" && key != "encrypted_password" && key != "options" {
				return errors.New("unknown excluded role configuration field")
			}
		}
		if options := row["options"]; options != nil {
			values, ok := options.([]any)
			if !ok || len(values) != 0 {
				return errors.New("original excluded role options changed")
			}
		}
	}
	if deltas := spec["delta_operations"]; deltas != nil {
		values, ok := deltas.([]any)
		if !ok {
			return errors.New("original fork delta inventory rejected")
		}
		for _, item := range values {
			row, ok := item.(map[string]any)
			if !ok {
				return errors.New("original fork delta entry rejected")
			}
			if _, excluded := digests[stringVal(row["name"])]; excluded {
				return errors.New("excluded role has a pending native delta")
			}
			if _, excluded := digests[stringVal(row["new_name"])]; excluded {
				return errors.New("excluded role has a pending native rename delta")
			}
		}
	}
	cluster["roles"] = kept
	return nil
}
func filterCatalogForkRoute(routes map[string]map[string]any, b catalogForkBacking) error {
	if !validCatalogForkDigests(b.Digests) {
		return errors.New("bounded original fork route credential proof required")
	}
	route := routes[selector(b.EndpointID)]
	if route == nil || route["project_id"] != b.ProjectID || route["branch_id"] != b.BranchID || route["role"] != "cloud_admin" {
		return errors.New("original excluded fork Proxy route scope rejected")
	}
	roles, ok := route["roles"].(map[string]any)
	if !ok {
		return errors.New("original fork Proxy roles unavailable")
	}
	for name, digest := range b.Digests {
		if value, present := roles[name]; present && catalogMarkerDigest(stringVal(value)) != digest {
			return errors.New("original excluded Proxy credential intent changed")
		}
	}
	for name := range b.Digests {
		delete(roles, name)
	}
	return nil
}
func (s *server) applyCatalogForkBacking(ctx context.Context, b catalogForkBacking, route bool) error {
	name, key, uid := kubeName(b.EndpointID)+"-config", "config.json", b.ConfigUID
	if route {
		name, key, uid = routesSecret, "routes.json", b.RoutesUID
	}
	if !functionResourceUID.MatchString(uid) || !functionWriterEndpointID.MatchString(b.EndpointID) || !validCatalogForkDigests(b.Digests) {
		return errors.New("original excluded fork backing identity rejected")
	}
	for attempt := 0; attempt < 7; attempt++ {
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", name), nil)
		if err != nil || stringVal(nested(secret, "metadata", "uid")) != uid || !route && !owned(secret, b.ProjectID, b.EndpointID) {
			return errors.New("original excluded fork backing UID or ownership changed")
		}
		raw, err := secretText(secret, key)
		if err != nil {
			return err
		}
		var value map[string]any
		if json.Unmarshal([]byte(raw), &value) != nil {
			return errors.New("original excluded fork backing JSON rejected")
		}
		before, _ := json.Marshal(value)
		if route {
			var routes map[string]map[string]any
			if json.Unmarshal([]byte(raw), &routes) != nil {
				return errors.New("original excluded fork route JSON rejected")
			}
			if err = filterCatalogForkRoute(routes, b); err != nil {
				return err
			}
			filtered, _ := json.Marshal(routes)
			if err = json.Unmarshal(filtered, &value); err != nil {
				return err
			}
		} else if err = filterCatalogForkConfig(value, b.Digests); err != nil {
			return err
		}
		after, _ := json.Marshal(value)
		if string(before) == string(after) {
			return nil
		}
		if attempt == 6 {
			return errors.New("original excluded fork CAS confirmation exhausted")
		}
		secret["data"].(map[string]any)[key] = base64.StdEncoding.EncodeToString(after)
		_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("secret", name), secret)
		var conflict kubeError
		if errors.As(err, &conflict) && conflict.Status == 409 {
			continue
		}
		if err != nil {
			return err
		}
	}
	return errors.New("original excluded fork backing not confirmed")
}
func verifyCatalogForkBackingScopeTx(ctx context.Context, tx pgx.Tx, operation, worker string, p createPayload) error {
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT pr.state='ready' AND pr.source='managed' AND pr.deleted_at IS NULL AND org.state='active' AND b.deleted_at IS NULL AND e.deleted_at IS NULL AND pr.tenant_id=$4 AND b.timeline_id=$5
 FROM projects pr JOIN organizations org ON org.id=pr.org_id JOIN branches b ON b.project_id=pr.id JOIN endpoints e ON e.branch_id=b.id AND e.project_id=pr.id WHERE pr.id=$1 AND b.id=$2 AND e.id=$3 FOR UPDATE OF pr,b,e`, p.ProjectID, p.BranchID, p.EndpointID, p.TenantID, p.TimelineID).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return errors.New("original fork backing parent or native scope changed")
	}
	if err = assertCreateLeaseTx(ctx, tx, operation, worker); err != nil {
		return err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM operations WHERE id=$1 AND project_id=$2 AND action IN ('create_branch','create_endpoint') AND (resource_id=$3 OR resource_id=$4)`, operation, p.ProjectID, p.BranchID, p.EndpointID).Scan(&raw)
	var original createPayload
	if err != nil || json.Unmarshal(raw, &original) != nil || original.ProjectID != p.ProjectID || original.BranchID != p.BranchID || original.EndpointID != p.EndpointID || original.TenantID != p.TenantID || original.TimelineID != p.TimelineID {
		return errors.New("original fork backing Operation scope rejected")
	}
	return nil
}
func readCatalogForkBacking(ctx context.Context, db connectionDefaultReader, endpoint string) (*catalogForkBacking, error) {
	var b catalogForkBacking
	var raw []byte
	err := db.QueryRow(ctx, `SELECT endpoint_id,branch_id,operation_id,config_uid,routes_uid,verifier_digests,state FROM catalog_fork_exclusion_backings WHERE endpoint_id=$1`, endpoint).Scan(&b.EndpointID, &b.BranchID, &b.OperationID, &b.ConfigUID, &b.RoutesUID, &raw, &b.State)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &b.Digests); err != nil {
		return nil, err
	}
	return &b, nil
}
func (s *server) freezeCatalogForkBacking(ctx context.Context, operation, worker string, p createPayload, excluded []catalogForkExclusion) (catalogForkBacking, error) {
	b := catalogForkBacking{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID, OperationID: operation, Digests: map[string]string{}}
	prior, err := readCatalogForkBacking(ctx, s.db, p.EndpointID)
	if err != nil {
		return b, err
	}
	for _, v := range excluded {
		secret, e := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", v.CredentialRef), nil)
		if e != nil || stringVal(nested(secret, "metadata", "uid")) != v.CredentialUID || nested(secret, "metadata", "labels", "neon-control/project-id") != p.ProjectID || secret["immutable"] != true {
			return b, errors.New("original excluded immutable credential UID rejected")
		}
		verifier, e := secretText(secret, "verifier")
		if e != nil || !functionsql.ValidVerifier(verifier) {
			return b, errors.New("original excluded opaque verifier intent rejected")
		}
		b.Digests[v.Name] = catalogMarkerDigest(verifier)
	}
	if prior != nil {
		prior.ProjectID = p.ProjectID
		old, _ := json.Marshal(prior.Digests)
		current, _ := json.Marshal(b.Digests)
		if prior.BranchID != p.BranchID || prior.OperationID != operation || string(old) != string(current) {
			return b, errors.New("original fork backing receipt or credential scope changed")
		}
		return *prior, nil
	}
	config, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(p.EndpointID)+"-config"), nil)
	if err != nil || !owned(config, p.ProjectID, p.EndpointID) {
		return b, errors.New("original fork config ownership required")
	}
	b.ConfigUID = stringVal(nested(config, "metadata", "uid"))
	created, err := time.Parse(time.RFC3339Nano, stringVal(nested(config, "metadata", "creationTimestamp")))
	if err != nil {
		return b, errors.New("original fork config creation time required")
	}
	routes, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
	if err != nil {
		return b, err
	}
	b.RoutesUID = stringVal(nested(routes, "metadata", "uid"))
	if !functionResourceUID.MatchString(b.ConfigUID) || !functionResourceUID.MatchString(b.RoutesUID) || !validCatalogForkDigests(b.Digests) {
		return b, errors.New("original excluded fork binding inventory required")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return b, err
	}
	defer tx.Rollback(ctx)
	if err = verifyCatalogForkBackingScopeTx(ctx, tx, operation, worker, p); err != nil {
		return b, err
	}
	var withinOriginalCreation bool
	err = tx.QueryRow(ctx, `SELECT $2>=o.created_at-interval '5 seconds' AND $2<=step.updated_at FROM operations o JOIN operation_steps step ON step.operation_id=o.id WHERE o.id=$1 AND step.name='create_neonvm' AND step.state='succeeded'`, operation, created).Scan(&withinOriginalCreation)
	if err != nil || !withinOriginalCreation {
		return b, errors.New("fork config was not present within original completed creation step")
	}
	frozen, _ := json.Marshal(b.Digests)
	_, err = tx.Exec(ctx, `INSERT INTO catalog_fork_exclusion_backings(endpoint_id,branch_id,operation_id,config_uid,routes_uid,verifier_digests,state) VALUES($1,$2,$3,$4,$5,$6,'updating') ON CONFLICT(endpoint_id) DO NOTHING`, p.EndpointID, p.BranchID, operation, b.ConfigUID, b.RoutesUID, frozen)
	if err != nil {
		return b, err
	}
	stored, err := readCatalogForkBacking(ctx, tx, p.EndpointID)
	if err != nil {
		return b, err
	}
	if stored == nil {
		return b, errors.New("original fork backing binding missing")
	}
	current, _ := json.Marshal(stored.Digests)
	if stored.OperationID != operation || stored.BranchID != p.BranchID || stored.ConfigUID != b.ConfigUID || stored.RoutesUID != b.RoutesUID || string(current) != string(frozen) {
		return b, errors.New("original fork backing binding conflict")
	}
	if err = tx.Commit(ctx); err != nil {
		return b, err
	}
	b.State = stored.State
	return b, nil
}
func (s *server) confirmCatalogForkExclusionBackings(ctx context.Context, operation, worker string, p createPayload) error {
	excluded, err := readCatalogForkExclusions(ctx, s.db, p.ProjectID, p.BranchID)
	if err != nil {
		return err
	}
	if len(excluded) == 0 {
		return nil
	}
	conn, err := s.catalogConnection(ctx, catalogPayload{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID})
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	verify := func() error {
		snapshot, e := readCatalogNativeSnapshot(ctx, conn, catalogNativeScope{TenantID: p.TenantID, TimelineID: p.TimelineID})
		if e != nil {
			return e
		}
		native := map[string]uint32{}
		for _, v := range snapshot.Roles {
			native[v.Name] = v.OID
		}
		for _, v := range excluded {
			if native[v.Name] != v.ObservedOID {
				return errors.New("copied excluded native role name or OID changed")
			}
		}
		return nil
	}
	if err = verify(); err != nil {
		return err
	}
	b, err := s.freezeCatalogForkBacking(ctx, operation, worker, p, excluded)
	if err != nil {
		return err
	}
	if err = s.applyCatalogForkBacking(ctx, b, false); err != nil {
		return err
	}
	if err = s.applyCatalogForkBacking(ctx, b, true); err != nil {
		return err
	}
	if err = verify(); err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = commitCatalogForkBackingReadyTx(ctx, tx, operation, worker, p, b); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func commitCatalogForkBackingReadyTx(ctx context.Context, tx pgx.Tx, operation, worker string, p createPayload, b catalogForkBacking) error {
	if err := verifyCatalogForkBackingScopeTx(ctx, tx, operation, worker, p); err != nil {
		return err
	}
	stored, err := readCatalogForkBacking(ctx, tx, p.EndpointID)
	if err != nil {
		return err
	}
	if stored == nil || stored.BranchID != p.BranchID || stored.OperationID != operation || stored.ConfigUID != b.ConfigUID || stored.RoutesUID != b.RoutesUID {
		return errors.New("original fork backing result identity rejected")
	}
	expected, _ := json.Marshal(b.Digests)
	actual, _ := json.Marshal(stored.Digests)
	if string(expected) != string(actual) {
		return errors.New("original fork backing result credential intent changed")
	}
	tag, err := tx.Exec(ctx, `UPDATE catalog_fork_exclusion_backings SET state='ready',updated_at=clock_timestamp() WHERE endpoint_id=$1 AND branch_id=$2 AND operation_id=$3 AND config_uid=$4 AND routes_uid=$5 AND state='updating'`, p.EndpointID, p.BranchID, operation, b.ConfigUID, b.RoutesUID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_type,resource_id,request_id,outcome) SELECT actor_id,'filter_inherited_role_projection','branch', $2,request_id,'succeeded' FROM operations WHERE id=$1`, operation, p.BranchID)
		if err != nil {
			return err
		}
	}
	return nil
}
