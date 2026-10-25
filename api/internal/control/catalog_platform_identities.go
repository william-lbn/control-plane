package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

// Platform identities are observed separately from customer CRUD. Establishing
// a baseline confers no permission to recreate or rename a protected object.
type catalogPlatformSource struct {
	Project, Branch, Endpoint               string
	CredentialRef, CredentialUID, ConfigUID string
	Role, Database                          string
	Scope                                   catalogNativeScope
}

type catalogPlatformObject struct {
	Expected     catalogIdentityExpectation `json:"expected"`
	MarkerSHA256 string                     `json:"marker_sha256"`
}

type catalogPlatformObservation struct {
	Version int                     `json:"version"`
	Source  catalogPlatformSource   `json:"source"`
	Objects []catalogPlatformObject `json:"objects"`
}

var errCatalogPlatformIdentity = errors.New("original platform catalog identity unavailable")

func (v catalogPlatformSource) valid() bool {
	return v.Project != "" && dataBranchID.MatchString(v.Branch) && v.Endpoint != "" && v.CredentialRef != "" && functionResourceUID.MatchString(v.CredentialUID) && functionResourceUID.MatchString(v.ConfigUID) && validPGIdentifier(v.Role) && validPGIdentifier(v.Database) && v.Scope.valid()
}

// Fetch the source from current owned Endpoint backings, never from a browser
// supplied UID. Only explicit platform/bootstrap names form the anchor set.
func captureCatalogPlatform(source catalogPlatformSource, snapshot catalogNativeSnapshot) (catalogPlatformObservation, error) {
	out := catalogPlatformObservation{Version: 1, Source: source, Objects: []catalogPlatformObject{}}
	if !source.valid() || source.Scope != snapshot.Scope || validateCatalogNativeSnapshot(snapshot) != nil {
		return out, errCatalogPlatformIdentity
	}
	roles := map[string]bool{source.Role: true, "control_probe": true}
	databases := map[string]bool{source.Database: true, "postgres": true, "template1": true}
	owners := map[uint32]string{}
	for _, role := range snapshot.Roles {
		owners[role.OID] = role.Name
	}
	for _, role := range snapshot.Roles {
		if !roles[role.Name] {
			continue
		}
		if !role.Flags.Login {
			return out, errCatalogPlatformIdentity
		}
		marker := catalogMarkerDigest(role.Marker)
		out.Objects = append(out.Objects, catalogPlatformObject{Expected: catalogIdentityExpectation{Scope: source.Scope, Kind: "role", Name: role.Name, OID: role.OID, MarkerSHA256: marker, RoleFlags: role.Flags}, MarkerSHA256: marker})
		delete(roles, role.Name)
	}
	for _, database := range snapshot.Databases {
		if !databases[database.Name] {
			continue
		}
		owner := owners[database.OwnerOID]
		if !validPGIdentifier(owner) {
			return out, errCatalogPlatformIdentity
		}
		marker := catalogMarkerDigest(database.Marker)
		out.Objects = append(out.Objects, catalogPlatformObject{Expected: catalogIdentityExpectation{Scope: source.Scope, Kind: "database", Name: database.Name, OID: database.OID, OwnerOID: database.OwnerOID, OwnerName: owner, MarkerSHA256: marker, DatabaseFlags: database.Flags}, MarkerSHA256: marker})
		delete(databases, database.Name)
	}
	if len(roles) != 0 || len(databases) != 0 || len(out.Objects) < 3 || len(out.Objects) > 5 {
		return out, errCatalogPlatformIdentity
	}
	sort.Slice(out.Objects, func(i, j int) bool {
		a, b := out.Objects[i].Expected, out.Objects[j].Expected
		return catalogInventoryKey(a.Kind, a.Name) < catalogInventoryKey(b.Kind, b.Name)
	})
	return out, nil
}

func (v catalogPlatformObservation) witness() (string, error) {
	if v.Version != 1 || !v.Source.valid() || len(v.Objects) < 3 || len(v.Objects) > 5 {
		return "", errCatalogPlatformIdentity
	}
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > 16384 {
		return "", errCatalogPlatformIdentity
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func compareCatalogPlatform(original catalogPlatformObservation, current catalogNativeSnapshot) ([]catalogIdentityDifference, error) {
	if _, err := original.witness(); err != nil {
		return nil, err
	}
	items := make([]catalogIdentityDifference, 0, len(original.Objects))
	anchors := map[string]bool{"role:" + original.Source.Role: true, "role:control_probe": true, "database:" + original.Source.Database: true, "database:postgres": true, "database:template1": true}
	if len(anchors) != len(original.Objects) {
		return nil, errCatalogPlatformIdentity
	}
	seenNames, seenOIDs := map[string]bool{}, map[string]map[uint32]bool{"role": {}, "database": {}}
	for _, object := range original.Objects {
		expected := object.Expected
		expected.MarkerSHA256 = object.MarkerSHA256
		if !anchors[catalogInventoryKey(expected.Kind, expected.Name)] || expected.Scope != original.Source.Scope || seenOIDs[expected.Kind] == nil || seenNames[catalogInventoryKey(expected.Kind, expected.Name)] || seenOIDs[expected.Kind][expected.OID] {
			return nil, errCatalogPlatformIdentity
		}
		seenNames[catalogInventoryKey(expected.Kind, expected.Name)] = true
		seenOIDs[expected.Kind][expected.OID] = true
		item := compareCatalogNativeIdentity(expected, current)
		if item.Status == "binding_invalid" {
			return nil, errCatalogPlatformIdentity
		}
		items = append(items, item)
	}
	return items, nil
}
