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

func TestMigration069FailedCreateCleanupRecognizesDSGAV1(t *testing.T) {
	raw, err := migrations.ReadFile("migrations/900000073_dsga_failed_create_cleanup_actions.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"action IN ('create_'||rk,'create_'||rk||'_dsga_v1')",
		"action IN ('create_endpoint','create_endpoint_dsga_v1')",
		"lease_owner IS NULL",
		"lease_expires_at IS NULL",
	} {
		if !bytes.Contains(raw, []byte(fragment)) {
			t.Fatal("DSGA failed-create cleanup migration lost required guard", fragment)
		}
	}
}
