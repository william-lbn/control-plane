package control

import (
	"bytes"
	"testing"
)

func TestMigrationRegistryRejectsUnreviewedNumericCollisions(t *testing.T) {
	known := []string{"058_catalog_current_observations.sql", "058_dsga_creation_identity.sql", dsga058JoinMigration}
	if err := validateMigrationRegistry(known); err != nil {
		t.Fatal(err)
	}
	for name, names := range map[string][]string{
		"unknown_duplicate": {"001_one.sql", "001_other.sql"},
		"missing_join":      known[:2],
		"third_058":         append(append([]string{}, known...), "058_other.sql"),
		"wrong_058":         {"058_catalog_current_observations.sql", "058_other.sql", dsga058JoinMigration},
		"zero":              {"000_bad.sql"},
		"negative":          {"-1_bad.sql"},
		"int_overflow":      {"2147483648_bad.sql"},
		"not_numeric":       {"future_bad.sql"},
	} {
		t.Run(name, func(t *testing.T) {
			if validateMigrationRegistry(names) == nil {
				t.Fatal("unreviewed migration registry was accepted")
			}
		})
	}
}

func TestMigration058JoinPreservesBothOriginalScripts(t *testing.T) {
	joined, err := migrations.ReadFile("migrations/" + dsga058JoinMigration)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"058_catalog_current_observations.sql", "058_dsga_creation_identity.sql"} {
		raw, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Count(joined, raw) != 1 {
			t.Fatal("forward join no longer embeds exactly one complete original script", name)
		}
	}
}
