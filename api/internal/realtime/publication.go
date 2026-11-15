package realtime

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

var ErrPublicationChanged = errors.New("logical publication differs from the authorized set of tables")

// VerifyPublication reads the current publication definition from the bound
// Writer. Every change to a registered table must produce an invalidation.
// A database-wide or schema-wide publication can grow behind the registered
// set, and a row filter or partition-root truncation can omit changes.
func VerifyPublication(ctx context.Context, conn *pgx.Conn, name string, relations []Relation) error {
	if !replicationName.MatchString(name) || len(relations) == 0 {
		return ErrPublicationChanged
	}
	var same bool
	err := conn.QueryRow(ctx, `
WITH published AS (
 SELECT jsonb_agg(jsonb_build_object('Schema',t.schemaname,'Name',t.tablename)) AS relations,
        bool_and(t.rowfilter IS NULL AND (NOT p.pubviaroot OR c.relkind<>'p')) AS complete
 FROM pg_catalog.pg_publication p
 JOIN pg_catalog.pg_publication_tables t ON t.pubname=p.pubname
 JOIN pg_catalog.pg_namespace n ON n.nspname=t.schemaname
 JOIN pg_catalog.pg_class c ON c.relnamespace=n.oid AND c.relname=t.tablename
 WHERE p.pubname=$1 AND NOT p.puballtables AND p.pubinsert AND p.pubupdate AND p.pubdelete AND p.pubtruncate
 AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_publication_namespace pn WHERE pn.pnpubid=p.oid)
)
SELECT COALESCE(complete AND relations <@ $2::jsonb AND $2::jsonb <@ relations
      AND jsonb_array_length(relations)=jsonb_array_length($2::jsonb),false) FROM published`,
		name, relations).Scan(&same)
	if err != nil || !same {
		return ErrPublicationChanged
	}
	return nil
}
