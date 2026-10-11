package functionsql

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var errRuntimeSession = errors.New("Functions restricted SQL session identity or permission boundary rejected")

// VerifyRuntimeSession observes an already authenticated dedicated connection.
// The Driver must first verify the immutable credential, original Writer route,
// proxy TLS hostname/CA and current leased branch intent. A SQL marker alone is
// not proof of a physical branch, and this read does not fence later owner DDL.
// No customer DSN, SQL text, password or underlying server error is returned.
// An active transaction is rejected rather than committed or rolled back here.
func VerifyRuntimeSession(ctx context.Context, conn *pgx.Conn, s Scope) error {
	if conn == nil || conn.IsClosed() || conn.PgConn().TxStatus() != 'I' || s.Validate() != nil {
		return errRuntimeSession
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return errRuntimeSession
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = tx.Rollback(cleanup)
	}()
	var matching bool
	err = tx.QueryRow(ctx, `SELECT current_database()=$1 AND session_user::text=$2 AND current_user::text=$2
 AND current_setting('default_transaction_read_only')='off'
 AND current_setting('statement_timeout')::interval=interval '30 seconds'
 AND current_setting('idle_session_timeout')::interval=interval '5 seconds'
 AND current_schemas(false)=ARRAY[$3::name,'pg_catalog'::name]
 AND EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$2 AND rolcanlogin AND rolconnlimit=4
 AND COALESCE(shobj_description(oid,'pg_authid'),'')=$4)`, s.Database, s.Role(), s.Schema, s.marker()).Scan(&matching)
	if err != nil || !matching || verifyRole(ctx, tx, s) != nil {
		return errRuntimeSession
	}
	// A new object cannot silently inherit permission to execute. Reconciliation
	// must explicitly grant all intended existing ordinary tables/sequences;
	// absence of a table is valid for code that needs only built-in SQL functions.
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$2)
 AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$2 AND c.relkind='r'
 AND NOT (has_table_privilege($1,c.oid,'SELECT') AND has_table_privilege($1,c.oid,'INSERT')
 AND has_table_privilege($1,c.oid,'UPDATE') AND has_table_privilege($1,c.oid,'DELETE')))
 AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$2 AND c.relkind='S'
 AND NOT (has_sequence_privilege($1,c.oid,'USAGE') AND has_sequence_privilege($1,c.oid,'SELECT')))`, s.Role(), s.Schema).Scan(&matching)
	if err != nil || !matching || tx.Commit(ctx) != nil {
		return errRuntimeSession
	}
	return nil
}
