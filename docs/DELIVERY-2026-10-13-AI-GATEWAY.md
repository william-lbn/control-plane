# AI Gateway local real-inference delivery

## Verified result

Runtime source `73ae9bb611acf9d781c658506cfa717f568e934e`, OpenAPI 0.18.0,
unified Helm 0.1.26. Public Linux CI run **38408395171** passed all fourteen jobs,
including **769 Go/real PostgreSQL tests, zero failures and zero skips**, Web,
Auth, Functions, Helm and eight OCI publications. Each image was anonymously
verified on Linux and imported serially on all three nodes: 24 exact-digest checks.
The existing Neon/Postgres/autoscaling fork images and source were not modified.

Unified eight-stage Helm inspect/preflight/apply/verify and the installed
manifest/PVC/Secret audit passed. The metadata baseline was backed up before
additive migration 026; this backup is not claimed as a DR restore drill.
AI uses a separate non-superuser metadata role with column-limited reads: no user
password hashes, public-schema creation or Backend credential mutation. Gateway
has no Kubernetes token; API does not mount the provider key or AI DB credential.

## Real Linux UI, not a fixture response

Linux Job `publication-ui-20261013143156` passed the actual React UI suite,
with nine checkpoint groups. The upstream was the user's Windows loopback Ollama
running the downloaded real `qwen2.5:0.5b` model through an SSH reverse tunnel and
an authenticated non-root Linux laboratory relay. No Windows CI/build or global
firewall/Ollama configuration was used. This is local real inference, not an
OpenAI/Databricks account or commercial provider certification.

| Checkpoint | Result |
| --- | --- |
| UI approved model catalog | pass |
| UI nonstream real completion and settled usage | pass; 46 actual provider tokens |
| UI SSE completion and settled usage | pass; 46 actual provider tokens |
| UI one-time app credential and filtered public catalog | pass |
| App real completion and cross-branch denial | pass |
| Unknown model and unsupported tool fields | rejected |
| UI credential revocation reflected by gateway | rejected immediately on next request |
| Usage reload | persisted; three completed requests / 137 provider tokens |
| Owned test credential retirement | revoked; metadata/evidence retained |

No database Compute or Functions VM was created; all managed VM/Runner counts
remained zero. Failures preceding browser execution were orchestration admission
issues, retained separately; the first browser execution passed without retries.
The first candidate's unused Go capability declaration was caught by public CI
and never deployed. Proxy forwarding lost after the host reboot and a laboratory
service's unavailable UID were repaired; successful code/image/runtime evidence
belongs to the corrected source and explicit later transport attempts.

## Reproduce and continue

See [implementation](AI-GATEWAY-IMPLEMENTATION.md) for interfaces, model,
least-privilege role, route/Secret settings and gateway deployment. The retained
test project has a ready root/sibling and suspended compute. Run the fixed suite
`web/e2e/ai-gateway.spec.ts` with the standard protected Linux runner configuration
and an authorized `NEON_E2E_CREDENTIAL_PROJECT`; do not embed admin/provider keys.

For the local model transport, Windows may run:

```sh
ollama pull qwen2.5:0.5b
ssh -N -R 127.0.0.1:11435:127.0.0.1:11434 root@192.168.146.100
```

Keep host-key verification enabled. On Linux, run the optional compiled
`ai-provider-relay` as an existing non-root account with a protected random
`AI_RELAY_KEY_FILE`, fixed loopback `AI_RELAY_UPSTREAM=http://127.0.0.1:11435` and
laboratory bind address; match its key with the provider Secret. Do not expose
unauthenticated Ollama to the LAN. Retained local test transport must be running
for the Playground to call Windows Ollama; an unavailable tunnel gives a bounded
sanitized error and inference is not replayed automatically.

Remaining AI work: provider configuration/rotation UI, catalog governance,
Responses/Anthropic/Gemini/embeddings, financial budget/credit settlement,
distributed production HA and trusted full-path TLS. The code and local model
remove the prior blanket “real inference requires external API credentials”
blocker. Other service and production gates remain independent.
