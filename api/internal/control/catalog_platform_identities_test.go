package control

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func platformCatalogFixture() (catalogPlatformSource, catalogNativeSnapshot) {
	_, snapshot := inventoryFixture()
	snapshot.Roles = append(snapshot.Roles, catalogNativeRole{OID: 101, Name: "cloud_admin", Flags: catalogRoleFlags{Login: true, CreateDB: true, CreateRole: true, ConnectionLimit: -1}}, catalogNativeRole{OID: 102, Name: "control_probe", Marker: "private native comment not exposed", Flags: catalogRoleFlags{Login: true, Superuser: true, ConnectionLimit: -1}})
	snapshot.Databases = append(snapshot.Databases, catalogNativeDatabase{OID: 151, Name: "postgres", OwnerOID: 101, Flags: catalogDatabaseFlags{AllowConnections: true, ConnectionLimit: -1}}, catalogNativeDatabase{OID: 152, Name: "template1", OwnerOID: 101, Flags: catalogDatabaseFlags{AllowConnections: true, Template: true, ConnectionLimit: -1}})
	return catalogPlatformSource{Project: "prj_0123456789abcdef", Branch: "br_0123456789abcdef", Endpoint: "ep_0123456789abcdef", CredentialRef: "owned-original-credentials", CredentialUID: "11111111-1111-4111-8111-111111111111", ConfigUID: "22222222-2222-4222-8222-222222222222", Role: "cloud_admin", Database: "postgres", Scope: snapshot.Scope}, snapshot
}

func TestCatalogPlatformCaptureRequiresCompleteOriginalAnchors(t *testing.T) {
	source, snapshot := platformCatalogFixture()
	observed, err := captureCatalogPlatform(source, snapshot)
	if err != nil || len(observed.Objects) != 4 {
		t.Fatal("platform capture", len(observed.Objects), err)
	}
	witness, err := observed.witness()
	if err != nil || !budgetEvidence.MatchString(witness) {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(observed)
	if bytes.Contains(raw, []byte("private native comment")) {
		t.Fatal("raw marker retained")
	}
	for i, j := 0, len(snapshot.Roles)-1; i < j; i, j = i+1, j-1 {
		snapshot.Roles[i], snapshot.Roles[j] = snapshot.Roles[j], snapshot.Roles[i]
	}
	reordered, err := captureCatalogPlatform(source, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := reordered.witness()
	if other != witness {
		t.Fatal("unordered inventory changed an identical observation")
	}
	changed := source
	changed.CredentialUID = "33333333-3333-4333-8333-333333333333"
	next, _ := captureCatalogPlatform(changed, snapshot)
	nextWitness, _ := next.witness()
	if nextWitness == witness {
		t.Fatal("original credential UID not bound")
	}
	for _, edit := range []func(*catalogPlatformSource, *catalogNativeSnapshot){
		func(v *catalogPlatformSource, s *catalogNativeSnapshot) { v.ConfigUID = "untrusted" },
		func(v *catalogPlatformSource, s *catalogNativeSnapshot) { s.Scope.TimelineID = strings.Repeat("f", 32) },
		func(v *catalogPlatformSource, s *catalogNativeSnapshot) { s.Databases = s.Databases[:1] },
		func(v *catalogPlatformSource, s *catalogNativeSnapshot) {
			for i := range s.Roles {
				if s.Roles[i].Name == "control_probe" {
					s.Roles[i].Flags.Login = false
				}
			}
		},
	} {
		a, b := platformCatalogFixture()
		edit(&a, &b)
		if _, err = captureCatalogPlatform(a, b); err == nil {
			t.Fatal("incomplete or mismatched platform captured")
		}
	}
}

func TestCatalogPlatformComparisonKeepsSystemOIDsAndOptions(t *testing.T) {
	source, snapshot := platformCatalogFixture()
	original, err := captureCatalogPlatform(source, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		label, kind, name, status string
		edit                      func(*catalogNativeSnapshot)
	}{
		{"unchanged", "role", "control_probe", "catalog_matches", func(*catalogNativeSnapshot) {}},
		{"renamed", "role", "control_probe", "renamed", func(s *catalogNativeSnapshot) {
			for i := range s.Roles {
				if s.Roles[i].OID == 102 {
					s.Roles[i].Name = "original_probe_moved"
				}
			}
		}},
		{"collision", "role", "control_probe", "name_collision", func(s *catalogNativeSnapshot) {
			for i := range s.Roles {
				if s.Roles[i].OID == 102 {
					s.Roles[i].Name = "original_probe_moved"
				}
			}
			s.Roles = append(s.Roles, catalogNativeRole{OID: 103, Name: "control_probe"})
		}},
		{"replaced", "role", "control_probe", "replaced", func(s *catalogNativeSnapshot) {
			for i := range s.Roles {
				if s.Roles[i].OID == 102 {
					s.Roles[i].OID = 103
				}
			}
		}},
		{"privilege", "role", "control_probe", "privilege_conflict", func(s *catalogNativeSnapshot) {
			for i := range s.Roles {
				if s.Roles[i].OID == 102 {
					s.Roles[i].Flags.Superuser = false
				}
			}
		}},
		{"default-database", "database", "postgres", "renamed", func(s *catalogNativeSnapshot) {
			for i := range s.Databases {
				if s.Databases[i].OID == 151 {
					s.Databases[i].Name = "original_default_moved"
				}
			}
		}},
		{"database-options", "database", "postgres", "database_options_changed", func(s *catalogNativeSnapshot) {
			for i := range s.Databases {
				if s.Databases[i].OID == 151 {
					s.Databases[i].Flags.AllowConnections = false
				}
			}
		}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			_, current := platformCatalogFixture()
			tc.edit(&current)
			items, e := compareCatalogPlatform(original, current)
			if e != nil {
				t.Fatal(e)
			}
			for _, item := range items {
				if item.Kind == tc.kind && item.ExpectedName == tc.name {
					if item.Status != tc.status || item.Credentials != "not_observed" {
						t.Fatal(item)
					}
					return
				}
			}
			t.Fatal("missing original platform object")
		})
	}
	encoded, _ := json.Marshal(original)
	var restored catalogPlatformObservation
	if json.Unmarshal(encoded, &restored) != nil {
		t.Fatal("decode baseline")
	}
	if _, err = compareCatalogPlatform(restored, snapshot); err != nil {
		t.Fatal("persisted marker hashes not restored", err)
	}
}
