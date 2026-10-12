# SQL workbench scripts

## 1. Product contract

The original `/query` and default **单条 SQL** behavior remain compatible.
New **SQL 脚本 · 逐条结果** uses
`POST /api/v1/projects/{project}/endpoints/{endpoint}/query-script` with the same
Editor/Admin authorization, CSRF, public database role/password, Proxy selector
and configured TLS as ordinary SQL. The body has `sql`, `password`, optional
`role` and `database`; no new infrastructure or Worker action is needed.

PostgreSQL parses the full script via its simple-query protocol. The control plane
does not split semicolons, including in dollar-quoted bodies/comments/strings,
or inject transaction commands. It never automatically retries a submitted script.
See [PostgreSQL protocol flow](https://www.postgresql.org/docs/16/protocol-flow.html)
and the [official SQL Editor direction](https://neon.com/docs/get-started/query-with-neon-sql-editor).

## 2. Data model and outcome semantics

`SQLScriptResult` contains `outcome` (`completed` or `failed`), ordered `results`,
`statements_completed`, `results_truncated`, `open_transaction_discarded` and
optional sanitized `{code, sqlstate}`. Each result contains `ordinal`, `columns`,
`rows`, `command` and `truncated`, with safe text/data rendering in the Console.

| Case | Result |
| --- | --- |
| All commands complete, idle transaction | HTTP 200, completed |
| PostgreSQL statement error | HTTP 200, failed, SQLSTATE and prior bounded results |
| Earlier explicit COMMIT then later error | Failed; earlier committed data may remain; do not claim global rollback |
| Unclosed transaction | Failed, `sql_script_transaction_open`, explicit discard flag; connection close rolls it back |
| Submitted transport/wire/decode failure | HTTP 503 `sql_script_outcome_unknown`, `retryable=false`; verify data before any retry |
| Failure before connection/dispatch | Existing sanitized connection error; no script sent |

Input is at most 32000 bytes. Retained output is at most 16 result sets,
64 columns per set, 200 total rows and 1 MiB encoded cell JSON (plus bounded
metadata). An 8 MiB wire body cap prevents an unbounded single network message.
Display truncation drains remaining results and does **not** skip later commands.
Unsafe JavaScript int8 integers and non-finite floats are projected as exact
strings. Rows/commands are not execution or billing metrics. Audit stores only
actor/endpoint/outcome; SQL/passwords are not stored in metadata or platform logs.

```mermaid
sequenceDiagram
    participant U as Console
    participant A as Go API
    participant P as Neon Proxy
    participant D as PostgreSQL
    U->>A: Explicit script POST + one-request password
    A->>P: Authenticate original selector/public role
    P->>D: One full simple-query script
    D-->>A: Native ordered results / error / transaction status
    A->>A: Bound display; sanitize failure; no replay
    A->>P: Close connection, discard open transaction
    A-->>U: Completed/failed result sets or unknown-outcome error
```

The Console clears results on scope/connection-target/mode changes and prevents
late responses from another project/Endpoint replacing the current view.
Passwords are cleared after one request, including failures. Results are escaped
React text/data; SQL output never becomes executable Console HTML.

## 3. Focused acceptance

`sql_script_integration_test.go` exercises actual isolated PostgreSQL result sets,
dollar quoting, writes, implicit error rollback, earlier explicit COMMIT,
unclosed transaction, retained result/row/cell budgets and exact numeric JSON.
API authorization/protected-target negatives must fail before external SQL.

The Linux `sql-script.spec.ts` suite reuses the accepted Owner project/database/
role. It creates one retained owned probe table, tests real multi-statement UI
results, failure semantics, independent observations of commit/rollback,
bounded output and exact bigint/non-finite display. It leaves data/evidence and
normally suspends Compute. It does not recreate the Owner fixture or rerun its
Writer/Reader lifecycle suite. A failure preserves the original table and request
records; do not blindly rerun CREATE or DML after an unknown outcome.

Protected fixture: `project_id`, `branch_id`, `writer_id`, `database`, `role` and
`password_file`, all outside Git. Use the trusted Linux workflow and protected
Console credentials. Product code is Go/TypeScript; private Python transport is
not shipped. **Actual Linux UI acceptance passed on its first run** at source
`3b0eb7c` / Helm 0.1.23: six checkpoint groups, normal Compute zero and retained
data. See the [exact version-specific receipt](https://github.com/william-lbn/neon-helm/blob/v0.1.23/docs/ACCEPTANCE-2026-10-13-SQL-SCRIPTS.md).

## 4. Independent production gates

This does not qualify distributed SQL admission/fencing, per-tenant concurrency,
Query Gateway HA, history/download/export, arbitrary retryable SQL workflows,
whole-chain trusted TLS or full official Console parity. Transport loss cannot
be converted into exactly-once writes by an HTTP retry or an idempotency header.
