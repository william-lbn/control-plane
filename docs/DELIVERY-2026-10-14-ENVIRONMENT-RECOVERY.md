# 2026-10-14: CNI executable failure and environment recovery

## Observed fault and impact

After the 19:29–19:30 local node restarts, node2 reported Ready but its local `/opt/cni/bin/loopback` segfaulted even for `CNI_COMMAND=VERSION`. New Pod sandboxes could not be created; existing metadata PostgreSQL, Pageserver, Worker, Adapter, gateway and Safekeeper workloads could not recover. The API then failed with connection refused to its metadata database. Pod/Node Ready summaries alone did not identify the host executable failure.

At inspection, each node had about 9.5 GiB available memory, node1's filesystem had about 386 GiB free (18% used), and there was no MemoryPressure or DiskPressure. This incident's immediate cause was a failing host CNI executable. It is not evidence that all past I/O problems have the same cause.

The healthy node1 and node3 loopback files were byte-identical, SHA-256 `662ee35e555fc5b1e6079ed453f364ddcf513c87503f83eafaa1905b293cc87d`, and VERSION exited 0. Node2's file had SHA-256 `00f632b2c5cbb953ba302e1a9eeb2d16da1b1b9cd54a5b183022933f57ab43ca`, the same length but 613,909 different bytes, and exited with SIGSEGV. Kernel logs independently confirmed the crashes. Why those bytes differed is not yet proved: interrupted installation, host-file modification, storage corruption and memory/virtualization faults require separate evidence. Do not label a physical hardware root cause without that evidence.

## Precise repairs

1. Retain node/pod manifests, events, kernel fault lines, hashes, original executable bytes and API failure logs in protected evidence.
2. Verify healthy peer files against each other. Back up node2's exact failing file.
3. Stage the verified same-version loopback, preserve ownership/mode, fsync and atomically replace only that path. Verify its hash and VERSION result again. No RKE2/etcd restart, network ADD/DEL or database rebuild is required.
4. Let the existing controllers recover their existing workloads. Retire only the exact startup-backoff API Pod with UID/resourceVersion preconditions after metadata PostgreSQL is Ready, preserving its manifest/logs; its Deployment recreates it.
5. Audit all ordinary CNI helpers. Four additional failing DHCP/firewall files on node1/node3 were found (SIGSEGV or nonzero VERSION). Download the pinned official CNI Plugins v1.9.1 archive, validate its published SHA-256, test the staged candidates, and atomically replace only the four proven failing helpers with backups. Functioning helper files and Cilium CNI remain unchanged.

Official archive: [CNI Plugins v1.9.1](https://github.com/containernetworking/plugins/releases/tag/v1.9.1), Linux amd64 archive SHA-256 `b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303`. Replacement DHCP SHA-256 `bf0552ff2ef54fbd8846b21ffe149f4de63dcd98d86d6b91de5e0bd94473870d`; firewall SHA-256 `ecbd112d77192a125e85ab1fa4ded6cfaf4e9732172e072ee248caa81eba7aed`. The unused helper repair is an explicitly pinned version replacement; it is not a claim that all existing host plugins have one version.

## Acceptance

- Three nodes Ready, each advertises `neonvm/kvm=1k`.
- 21 ordinary host CNI executable VERSION checks per node: **63 passed, zero nonzero exits**.
- Metadata PostgreSQL, Go API/Worker, Pageservers, Adapter, gateway, AI, Proxy, storage controller/broker and Safekeepers recovered Ready.
- Actual Console admin login HTTP 200, authenticated read of the original project `prj_6f3c51bd15b772e3` HTTP 200.
- No Neon/Postgres/autoscaling source changes; no PVC, Secret or application record deletion.
- Existing console runtime remains OpenAPI 0.21.0 / source be04cb4. Auth mail work is still a subsequent candidate until its own acceptance succeeds.

A first authenticated list request returned 404 because it omitted organization scope; the project-scoped read verified the retained project. The account policy was not weakened. An initial `/api/readyz` URL was not the API's actual readiness route; the direct API `/readyz` returned 200. Use actual service health and authenticated product routes.

## Repeatable diagnostics and prevention

On each Linux node, with the repository's pinned Node runtime:

```sh
node tools/audit-node-cni.mjs --directory /opt/cni/bin --output /protected/cni-report.json
```

This read-only tool executes VERSION with bounded timeouts, records file SHA-256, supported CNI versions and exit/signals, and verifies the file did not change during inspection. It does not execute ADD/DEL or make network changes. Protect executable backups and retain them separately. Compare hashes only after checking package/source/version; differing healthy versions do not justify replacement.

Include this check after node reboot or host networking maintenance and before starting Compute tests. Track expected installed package provenance and any manual replacement. Avoid installing plugins by overwriting live executables in place: stage, validate, retain the old file and atomically rename. Alert on CNI sandbox errors and metadata database unavailability even when Node Ready is true. Underlying file-integrity recurrence still needs host storage/memory investigation; current evidence does not establish its ultimate cause.

Private records are under `control-plane/production/evidence/20261014-environment`. Their binary/log contents are not part of the public source repository. The failing loopback also remains backed up on node2 under `/var/lib/neon-control/product/20261014/node2-cni-repair-2000/loopback.before`; the four helper backups remain on their original nodes under the corresponding `cni-unused-helpers-repair-2010` directory.
