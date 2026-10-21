package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
)

func defaultOwnerConfigRoleHash(config map[string]any, p defaultOwnerPayload) (string, error) {
	roles, ok := nested(config, "spec", "cluster", "roles").([]any)
	if !ok {
		return "", errors.New("default Owner role configuration unavailable")
	}
	expected := map[string]string{p.Native.PlatformRole: p.BootstrapVerifierSHA256, "control_probe": p.ProbeVerifierSHA256}
	if p.Mode == "managed" {
		expected[p.Owner] = p.TargetVerifierSHA256
	}
	selected := map[string]map[string]any{}
	for _, item := range roles {
		role, ok := item.(map[string]any)
		if !ok {
			return "", errors.New("default Owner role configuration invalid")
		}
		name := stringVal(role["name"])
		digest, required := expected[name]
		if !required {
			continue
		}
		if selected[name] != nil || digest == "" || catalogMarkerDigest(stringVal(role["encrypted_password"])) != digest {
			return "", errors.New("original default Owner credential configuration rejected")
		}
		options := role["options"]
		if options != nil {
			values, empty := options.([]any)
			if !empty || len(values) != 0 {
				return "", errors.New("original platform/Owner role options rejected")
			}
		}
		selected[name] = role
	}
	if len(selected) != len(expected) {
		return "", errors.New("original platform, probe or target Owner role missing")
	}
	raw, err := json.Marshal(selected)
	if err != nil {
		return "", err
	}
	return catalogMarkerDigest(string(raw)), nil
}
func defaultOwnerConfiguredOwner(config map[string]any, name string) (string, error) {
	values, ok := nested(config, "spec", "cluster", "databases").([]any)
	if !ok {
		return "", errors.New("default Owner database configuration unavailable")
	}
	owner := ""
	found := false
	for _, item := range values {
		row, ok := item.(map[string]any)
		if !ok {
			return "", errors.New("default Owner database configuration invalid")
		}
		if row["name"] == name {
			if found || !validPGIdentifier(stringVal(row["owner"])) {
				return "", errors.New("duplicate or invalid default database configuration")
			}
			found = true
			owner = stringVal(row["owner"])
		}
	}
	return owner, nil
}
func defaultOwnerConfigDelta(config map[string]any, p defaultOwnerPayload, endpoint string) error {
	cluster, ok := nested(config, "spec", "cluster").(map[string]any)
	if !ok {
		return errors.New("default Owner compute cluster unavailable")
	}
	current, err := defaultOwnerConfiguredOwner(config, p.Name)
	if err != nil {
		return err
	}
	original, known := p.ColdOwners[endpoint]
	if !known || (current != original && current != p.Owner) {
		return errors.New("original default database cold intent changed")
	}
	if current == "" {
		// Only a previously absent entry can be inserted. An unknown removal of a
		// bound existing entry is not silently adopted on retry.
		if original != "" {
			return errors.New("original default database cold entry disappeared")
		}
		values := cluster["databases"].([]any)
		cluster["databases"] = append(values, map[string]any{"name": p.Name, "owner": p.Owner, "options": nil})
	} else {
		for _, item := range cluster["databases"].([]any) {
			row := item.(map[string]any)
			if row["name"] == p.Name {
				row["owner"] = p.Owner
			}
		}
	}
	return nil
}
func (s *server) verifyDefaultOwnerRoutes(ctx context.Context, p defaultOwnerPayload) error {
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
	if err != nil || stringVal(nested(secret, "metadata", "uid")) != p.RoutesUID {
		return errors.New("original default Owner Proxy routes UID rejected")
	}
	raw, err := secretText(secret, "routes.json")
	if err != nil {
		return err
	}
	var routes map[string]map[string]any
	if json.Unmarshal([]byte(raw), &routes) != nil {
		return errors.New("default Owner Proxy routes invalid")
	}
	expected := map[string]string{p.Native.PlatformRole: p.BootstrapVerifierSHA256, "control_probe": p.ProbeVerifierSHA256}
	if p.Mode == "managed" {
		expected[p.Owner] = p.TargetVerifierSHA256
	}
	for id := range p.ConfigUIDs {
		route := routes[selector(id)]
		if route == nil || route["project_id"] != p.ProjectID || route["branch_id"] != p.BranchID || route["role"] != p.Native.PlatformRole {
			return errors.New("original default Owner Proxy endpoint scope rejected")
		}
		roles, ok := route["roles"].(map[string]any)
		if !ok {
			return errors.New("default Owner Proxy role inventory unavailable")
		}
		for name, digest := range expected {
			if catalogMarkerDigest(stringVal(roles[name])) != digest {
				return errors.New("original default Owner Proxy credential intent rejected")
			}
		}
	}
	return nil
}
func (s *server) captureDefaultOwnerBackings(ctx context.Context, p *defaultOwnerPayload) error {
	endpoints, err := s.many(ctx, `SELECT id,role_name,database_name FROM endpoints WHERE project_id=$1 AND branch_id=$2 AND deleted_at IS NULL ORDER BY id`, p.ProjectID, p.BranchID)
	if err != nil {
		return err
	}
	if len(endpoints) == 0 || len(endpoints) > 128 {
		return errors.New("bounded retained default Owner endpoint inventory required")
	}
	p.ConfigUIDs = map[string]string{}
	p.RoleHashes = map[string]string{}
	p.ColdOwners = map[string]string{}
	for _, endpoint := range endpoints {
		id := stringVal(endpoint["id"])
		if endpoint["role_name"] != p.Native.PlatformRole || endpoint["database_name"] != p.Name {
			return errors.New("original default database endpoint bootstrap identity differs")
		}
		secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(id)+"-config"), nil)
		if err != nil || !owned(secret, p.ProjectID, id) {
			return errors.New("original default Owner compute config unavailable")
		}
		uid := stringVal(nested(secret, "metadata", "uid"))
		if !functionResourceUID.MatchString(uid) {
			return errors.New("original default Owner compute UID required")
		}
		raw, err := secretText(secret, "config.json")
		if err != nil {
			return err
		}
		var config map[string]any
		if json.Unmarshal([]byte(raw), &config) != nil {
			return errors.New("default Owner compute config invalid")
		}
		p.ConfigUIDs[id] = uid
		p.RoleHashes[id], err = defaultOwnerConfigRoleHash(config, *p)
		if err != nil {
			return err
		}
		p.ColdOwners[id], err = defaultOwnerConfiguredOwner(config, p.Name)
		if err != nil {
			return err
		}
		if original := p.ColdOwners[id]; original != "" && original != p.PreviousOwner && original != p.Owner {
			return renameFault(409, "default_owner_cold_intent_conflict", "Original default database cold Owner differs from both observed and requested Owner")
		}
	}
	routes, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
	if err != nil {
		return err
	}
	p.RoutesUID = stringVal(nested(routes, "metadata", "uid"))
	if !p.valid() {
		return errors.New("original default Owner payload bindings rejected")
	}
	return s.verifyDefaultOwnerRoutes(ctx, *p)
}
func (s *server) defaultOwnerSpecs(ctx context.Context, p defaultOwnerPayload, write, requireTarget bool) error {
	if !p.valid() {
		return errors.New("invalid default Owner bound configuration")
	}
	ids := make([]string, 0, len(p.ConfigUIDs))
	for id := range p.ConfigUIDs {
		ids = append(ids, id)
	}
	// Persist Reader intents first, then the Writer. A cold Writer must never
	// receive an old owner after the live native change has been committed.
	sort.Slice(ids, func(i, j int) bool {
		if ids[i] == p.EndpointID {
			return false
		}
		if ids[j] == p.EndpointID {
			return true
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		confirmed := false
		for attempt := 0; attempt < 7; attempt++ {
			secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(id)+"-config"), nil)
			if err != nil || !owned(secret, p.ProjectID, id) || stringVal(nested(secret, "metadata", "uid")) != p.ConfigUIDs[id] {
				return errors.New("original default Owner compute UID changed")
			}
			raw, err := secretText(secret, "config.json")
			if err != nil {
				return err
			}
			var config map[string]any
			if json.Unmarshal([]byte(raw), &config) != nil {
				return errors.New("default Owner compute JSON rejected")
			}
			digest, err := defaultOwnerConfigRoleHash(config, p)
			if err != nil {
				return err
			}
			if digest != p.RoleHashes[id] {
				return errors.New("original platform or selected Owner role configuration changed")
			}
			current, err := defaultOwnerConfiguredOwner(config, p.Name)
			if err != nil {
				return err
			}
			if !write {
				if requireTarget && current != p.Owner {
					return errors.New("default database cold Owner not confirmed")
				}
				if err = defaultOwnerConfigDelta(config, p, id); err != nil {
					return err
				}
				confirmed = true
				break
			}
			before, _ := json.Marshal(config)
			if err = defaultOwnerConfigDelta(config, p, id); err != nil {
				return err
			}
			after, _ := json.Marshal(config)
			if string(before) == string(after) {
				confirmed = true
				break
			}
			if attempt == 6 {
				return errors.New("default database Owner CAS confirmation exhausted")
			}
			secret["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(after)
			_, err = s.kube.request(ctx, http.MethodPut, s.kube.path("secret", kubeName(id)+"-config"), secret)
			var conflict kubeError
			if errors.As(err, &conflict) && conflict.Status == 409 {
				continue
			}
			if err != nil {
				return err
			}
			// Always read after a successful PUT; a disappeared reply is observed only
			// by a later retry of this same operation and original UID.
		}
		if !confirmed {
			return errors.New("default database cold Owner unconfirmed")
		}
	}
	return s.verifyDefaultOwnerRoutes(ctx, p)
}
