#!/usr/bin/env node
// Read-only native storage/resource proof. Browser receives no kube credentials.
import fs from "node:fs";
import path from "node:path";
import { parseArgs } from "node:util";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { setTimeout as delay } from "node:timers/promises";

const { values } = parseArgs({
  options: {
    "fixture-dir": { type: "string" },
    "evidence-dir": { type: "string" },
    scope: { type: "string" },
  },
});
if (
  process.platform !== "linux" ||
  !values["fixture-dir"] ||
  !values["evidence-dir"] ||
  !values.scope
)
  throw new Error("Explicit protected Linux inputs required");
const directory = path.resolve(values["fixture-dir"]),
  evidence = path.resolve(values["evidence-dir"]),
  owner = fs.lstatSync(directory),
  scopeFile = fs.lstatSync(values.scope);
if (
  !owner.isDirectory() ||
  owner.isSymbolicLink() ||
  owner.mode & 0o077 ||
  !scopeFile.isFile() ||
  scopeFile.isSymbolicLink() ||
  scopeFile.mode & 0o077
)
  throw new Error("Protected nonsymlink inputs required");
const scope = JSON.parse(fs.readFileSync(values.scope, "utf8"));
for (const [key, prefix] of [
  ["project_id", "prj"],
  ["branch_id", "br"],
  ["writer_id", "ep"],
])
  if (!new RegExp("^" + prefix + "_[a-f0-9]{16}$").test(scope[key]))
    throw new Error("Reviewed original scope required");
fs.mkdirSync(evidence, { mode: 0o700 });
const report = {
  result: "running",
  readOnly: true,
  metadataStatusChanged: false,
  performanceReliabilityCampaign: false,
  steps: [],
};
const save = () =>
  fs.writeFileSync(
    path.join(evidence, "result.json"),
    JSON.stringify(report, null, 2) + "\n",
    { mode: 0o600 },
  );
let aborted = false;
process.on("SIGTERM", () => {
  aborted = true;
});
process.on("SIGINT", () => {
  aborted = true;
});
function kube(args) {
  const server = process.env.NEON_E2E_KUBE_SERVER;
  if (!/^https:\/\/[a-zA-Z0-9.:-]+$/.test(server || ""))
    throw new Error("Explicit API server required");
  const result = spawnSync(
    process.env.KUBECTL_BIN || "kubectl",
    ["--server=" + server, "--request-timeout=20s", "-n", "neon", ...args],
    { encoding: "utf8", timeout: 30000, maxBuffer: 4 * 1024 * 1024 },
  );
  if (result.error || result.status !== 0)
    throw new Error("Read-only native observation unavailable");
  return result.stdout;
}
function read(kind, name) {
  return JSON.parse(kube(["get", kind, name, "-o", "json"]));
}
function sql(statement) {
  const quote = (value) => "'" + value.replaceAll("'", "'\"'\"'") + "'";
  return kube([
    "exec",
    "deployment/neon-control-v2-db",
    "--",
    "sh",
    "-c",
    'PGPASSWORD="$POSTGRES_PASSWORD" psql -U neon_control_v2 -d neon_control_v2 -At -v ON_ERROR_STOP=1 -c ' +
      quote(statement),
  ]).trim();
}
async function marker(name) {
  const deadline = Date.now() + 600000;
  while (Date.now() < deadline && !aborted) {
    if (fs.existsSync(path.join(directory, "branch-expiration-abort.json")))
      throw new Error("Owned observation interrupted");
    try {
      const file = path.join(directory, name),
        stat = fs.lstatSync(file);
      if (
        !stat.isFile() ||
        stat.isSymbolicLink() ||
        stat.mode & 0o077 ||
        stat.size > 4096
      )
        throw new Error("Unsafe marker");
      return JSON.parse(fs.readFileSync(file, "utf8"));
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
    }
    await delay(500);
  }
  throw new Error("Original observation deadline or interruption");
}
function ack(name, value) {
  const file = path.join(directory, name);
  fs.writeFileSync(file, JSON.stringify(value) + "\n", {
    mode: 0o600,
    flag: "wx",
  });
  if (process.getuid() === 0) fs.chownSync(file, owner.uid, owner.gid);
}
save();
try {
  const original = await marker("branch-expiration-observe.json");
  if (
    original.project_id !== scope.project_id ||
    original.parent_branch_id !== scope.branch_id ||
    !/^br_[a-f0-9]{16}$/.test(original.branch_id) ||
    !/^ep_[a-f0-9]{16}$/.test(original.endpoint_id) ||
    !/^op_[a-f0-9]{24}$/.test(original.create_operation_id) ||
    !/^([a-f0-9]{32})$/.test(original.timeline_id) ||
    original.branch_id === scope.branch_id
  )
    throw new Error("Exact original new child required");
  const name = "cp-" + original.endpoint_id.slice(3),
    identity = JSON.parse(
      sql(
        `SELECT jsonb_build_object('tenant_id',p.tenant_id,'timeline_id',b.timeline_id,'state',b.state) FROM projects p JOIN branches b ON b.project_id=p.id JOIN endpoints e ON e.branch_id=b.id JOIN operations o ON o.project_id=p.id WHERE p.id='${scope.project_id}' AND p.source='managed' AND p.state='ready' AND b.id='${original.branch_id}' AND b.parent_branch_id='${scope.branch_id}' AND NOT b.is_default AND e.id='${original.endpoint_id}' AND e.workload_kind='neonvm' AND o.id='${original.create_operation_id}' AND o.resource_type='branch' AND o.resource_id=b.id AND o.action='create_branch' AND o.state='succeeded';`,
      ),
    );
  if (
    identity.timeline_id !== original.timeline_id ||
    !/^([a-f0-9]{32})$/.test(identity.tenant_id)
  )
    throw new Error("Original native timeline scope mismatch");
  const secrets = () =>
    ["credentials", "config"].map((suffix) => {
      const value = read("secret", name + "-" + suffix),
        labels = value.metadata.labels;
      if (
        labels?.["neon-control/project-id"] !== scope.project_id ||
        labels?.["neon-control/endpoint-id"] !== original.endpoint_id ||
        !value.metadata.uid ||
        !value.data
      )
        throw new Error("Original Secret scope mismatch");
      return {
        name: name + "-" + suffix,
        uid: value.metadata.uid,
        data_sha256: createHash("sha256")
          .update(JSON.stringify(value.data, Object.keys(value.data).sort()))
          .digest("hex"),
      };
    });
  const timeline = () => {
    const value = JSON.parse(
      kube([
        "get",
        "--raw",
        `/api/v1/namespaces/neon/services/http:pageserver-managed:9898/proxy/v1/tenant/${identity.tenant_id}/timeline/${identity.timeline_id}`,
      ]),
    );
    if (value.timeline_id !== identity.timeline_id)
      throw new Error("Original native timeline response mismatch");
    return {
      tenant_id: identity.tenant_id,
      timeline_id: identity.timeline_id,
      ancestor_timeline_id: value.ancestor_timeline_id ?? null,
      ancestor_lsn: value.ancestor_lsn ?? null,
    };
  };
  let baseline;
  if (scope.branch_expiration_resume) {
    baseline = scope.branch_expiration_resume.original_observation;
    if (
      !baseline ||
      baseline.branch_id !== original.branch_id ||
      baseline.endpoint_id !== original.endpoint_id ||
      !Array.isArray(baseline.secret_identities) ||
      baseline.secret_identities.length !== 2
    )
      throw new Error("Reviewed original native observation required");
  } else {
    if (identity.state !== "ready")
      throw new Error("Original branch must be ready before scheduling");
    const service = read("service", name);
    if (
      service.metadata.labels?.["neon-control/project-id"] !==
        scope.project_id ||
      service.metadata.labels?.["neon-control/endpoint-id"] !==
        original.endpoint_id ||
      service.spec.selector?.["vm.neon.tech/name"] !== name ||
      !service.metadata.uid
    )
      throw new Error("Original Service scope mismatch");
    baseline = {
      branch_id: original.branch_id,
      endpoint_id: original.endpoint_id,
      service_uid: service.metadata.uid,
      secret_identities: secrets(),
      timeline: timeline(),
    };
  }
  if (
    JSON.stringify(secrets()) !== JSON.stringify(baseline.secret_identities) ||
    JSON.stringify(timeline()) !== JSON.stringify(baseline.timeline)
  )
    throw new Error("Original retained observation changed");
  report.originalObservation = baseline;
  report.steps.push("native_timeline_service_and_secret_identities_observed");
  save();
  ack("branch-expiration-storage-observed.json", {
    branch_id: original.branch_id,
    timeline_id: original.timeline_id,
  });
  const done = await marker("branch-expiration-finished.json");
  if (
    done.branch_id !== original.branch_id ||
    done.endpoint_id !== original.endpoint_id ||
    !/^op_[a-f0-9]{24}$/.test(done.expiration_operation_id)
  )
    throw new Error("Original expiration receipt required");
  const payload = JSON.parse(
    sql(
      `SELECT o.payload FROM operations o JOIN branches b ON b.deletion_operation_id=o.id WHERE o.id='${done.expiration_operation_id}' AND o.project_id='${scope.project_id}' AND o.resource_id='${original.branch_id}' AND o.action='expire_branch' AND o.state='succeeded' AND b.state='deleted' AND b.expiration_status='retired' AND b.expiration_operation_id=o.id;`,
    ),
  );
  if (
    payload.expected_service_uids?.[original.endpoint_id] !==
    baseline.service_uid
  )
    throw new Error("Original Service UID witness changed");
  if (
    kube([
      "get",
      "virtualmachines.vm.neon.tech",
      name,
      "--ignore-not-found",
      "-o",
      "json",
    ]).trim() ||
    kube(["get", "service", name, "--ignore-not-found", "-o", "json"]).trim()
  )
    throw new Error("Original Compute or Service remains");
  if (
    JSON.parse(
      kube(["get", "pods", "-l", "vm.neon.tech/name=" + name, "-o", "json"]),
    ).items.length
  )
    throw new Error("Original Runner remains");
  if (
    JSON.stringify(secrets()) !== JSON.stringify(baseline.secret_identities) ||
    JSON.stringify(timeline()) !== JSON.stringify(baseline.timeline)
  )
    throw new Error("Original native data identity or Secret changed");
  report.expirationOperationID = done.expiration_operation_id;
  report.steps.push(
    "original_expiration_completed_native_timeline_and_secrets_retained",
  );
  report.result = "pass";
  save();
  ack("branch-expiration-retained.json", {
    branch_id: original.branch_id,
    vm_count: 0,
    service_count: 0,
    timeline_retained: true,
    secrets_retained: true,
  });
} catch (error) {
  report.result = "fail";
  report.error = error.message;
  process.exitCode = 1;
} finally {
  save();
}
