# Control clients 0.1.0

CLI and stdio MCP clients for the bundled self-hosted OpenAPI contract. Requires Node.js 24.5+ in the 24.x line. Install with `npm ci --ignore-scripts --no-audit --no-fund`; run functional tests with `npm test`.

See the [configuration, permissions, limits and acceptance record](../docs/CONTROL-CLIENTS.md). Credentials belong in a protected operator profile outside this checkout, referenced by `NEON_CONTROL_PROFILE`. The default is read-only. Live integration entries require explicit protected Linux fixtures and are excluded from ordinary tests.
