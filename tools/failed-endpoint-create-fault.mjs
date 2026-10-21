#!/usr/bin/env node
// Trusted Linux operator: reject CREATE of one newly queued owned VM only.
// Browser code receives no Kubernetes credentials. No metadata status is forged.
import fs from "node:fs";
import path from "node:path";
import { parseArgs } from "node:util";
import { spawnSync } from "node:child_process";
import { setTimeout as delay } from "node:timers/promises";
import { createHash } from "node:crypto";

const { values } = parseArgs({
  options: {
    "fixture-dir": { type: "string" },
    "evidence-dir": { type: "string" },
    scope: { type: "string" },
    "api-servers": { type: "string" },
    "resource-kind": { type: "string", default: "endpoint" },
  },
});
if (
  process.platform !== "linux" ||
  !values["fixture-dir"] ||
  !values["evidence-dir"] ||
  !values.scope ||
  !values["api-servers"]
)
  throw new Error(
    "Explicit protected Linux paths and all API servers required",
  );
const apiServers = values["api-servers"].split(",");
if (
  !apiServers.length ||
  apiServers.some((x) => !/^https:\/\/[a-zA-Z0-9.:-]+$/.test(x))
)
  throw new Error("Invalid API server list");
const fixture = path.resolve(values["fixture-dir"]),
  evidence = path.resolve(values["evidence-dir"]);
const owner = fs.lstatSync(fixture),
  scopeStat = fs.lstatSync(values.scope);
if (
  !owner.isDirectory() ||
  owner.isSymbolicLink() ||
  owner.mode & 0o077 ||
  !scopeStat.isFile() ||
  scopeStat.isSymbolicLink() ||
  scopeStat.mode & 0o077
)
  throw new Error("Protected nonsymlink inputs required");
const scope = JSON.parse(fs.readFileSync(values.scope, "utf8"));
const resourceKind = values["resource-kind"],
  parentCleanup = resourceKind !== "endpoint";
if (!["endpoint", "project", "branch"].includes(resourceKind))
  throw new Error("Explicit supported creation kind required");
let targetProject = scope.project_id;
for (const [key, prefix] of [
  ["project_id", "prj"],
  ["branch_id", "br"],
  ["writer_id", "ep"],
])
  if (!new RegExp("^" + prefix + "_[a-f0-9]{16}$").test(scope[key]))
    throw new Error("Fixed retained scope required");
fs.mkdirSync(evidence, { mode: 0o700 });
const report = {
  result: "running",
  mode: "native_admission_rejection_for_one_owned_vm",
  steps: [],
  metadataStatusForged: false,
  dataDeleted: false,
  secretsChanged: false,
  resourceKind,
};
const save = () =>
  fs.writeFileSync(
    path.join(evidence, "result.json"),
    JSON.stringify(report, null, 2) + "\n",
    { mode: 0o600 },
  );
let aborted = false,
  paused = false,
  workerUID,
  policy,
  binding;
process.on("SIGTERM", () => {
  aborted = true;
});
process.on("SIGINT", () => {
  aborted = true;
});
const abortWatch = setInterval(() => {
  try {
    const file = path.join(fixture, "failed-endpoint-abort.json"),
      s = fs.lstatSync(file);
    if (
      s.isFile() &&
      !s.isSymbolicLink() &&
      !(s.mode & 0o077) &&
      s.size < 1024 &&
      JSON.parse(fs.readFileSync(file, "utf8")).abort === true
    )
      aborted = true;
  } catch (e) {
    if (e.code !== "ENOENT") aborted = true;
  }
}, 500);
function kube(
  args,
  {
    input,
    check = true,
    timeout = 45000,
    server = process.env.NEON_E2E_KUBE_SERVER,
  } = {},
) {
  if (server && !/^https:\/\/[a-zA-Z0-9.:-]+$/.test(server))
    throw new Error("Invalid explicit API server");
  const result = spawnSync(
    process.env.KUBECTL_BIN || "kubectl",
    [
      ...(server ? ["--server=" + server] : []),
      "--request-timeout=25s",
      "-n",
      "neon",
      ...args,
    ],
    { input, encoding: "utf8", timeout, maxBuffer: 4 * 1024 * 1024 },
  );
  if (result.error || (check && result.status !== 0)) {
    report.failedKubernetesCommand = {
      verb: args[0],
      resource: args[1],
      exit_code: result.status,
      signal: result.signal,
      stderr_sha256: createHash("sha256")
        .update(result.stderr || "")
        .digest("hex"),
    };
    save();
    throw new Error("Operator Kubernetes step failed");
  }
  return result;
}
function read(kind, name) {
  return JSON.parse(kube(["get", kind, name, "-o", "json"]).stdout);
}
function secretIdentities(endpoint) {
  return ["credentials", "config"].map((suffix) => {
    const name = "cp-" + endpoint.slice(3) + "-" + suffix,
      value = read("secret", name);
    if (
      value.metadata.labels?.["neon-control/project-id"] !== targetProject ||
      value.metadata.labels?.["neon-control/endpoint-id"] !== endpoint ||
      !value.metadata.uid ||
      !value.data
    )
      throw new Error("Owned retained Secret identity required");
    return {
      name,
      uid: value.metadata.uid,
      data_sha256: createHash("sha256")
        .update(JSON.stringify(value.data, Object.keys(value.data).sort()))
        .digest("hex"),
    };
  });
}
function sql(statement) {
  const quote = (s) => "'" + s.replaceAll("'", "'\"'\"'") + "'";
  return kube([
    "exec",
    "deployment/neon-control-v2-db",
    "--",
    "sh",
    "-c",
    'PGPASSWORD="$POSTGRES_PASSWORD" psql -U neon_control_v2 -d neon_control_v2 -At -v ON_ERROR_STOP=1 -c ' +
      quote(statement),
  ]).stdout.trim();
}
function creationMatch(queued) {
  const resource =
    resourceKind === "project"
      ? queued.project_id
      : resourceKind === "branch"
        ? queued.branch_id
        : queued.endpoint_id;
  return `o.id='${queued.operation_id}' AND o.project_id='${queued.project_id}' AND o.resource_type='${resourceKind}' AND o.resource_id='${resource}' AND o.action='create_${resourceKind}' AND e.id='${queued.endpoint_id}' AND e.branch_id='${queued.branch_id}' AND o.payload->>'endpoint_id'=e.id AND o.payload->>'branch_id'=e.branch_id AND e.endpoint_type='${parentCleanup ? "read_write" : "read_only"}'`;
}
function retainedTimeline(queued) {
  const identity = JSON.parse(
    sql(
      `SELECT jsonb_build_object('tenant_id',p.tenant_id,'timeline_id',b.timeline_id) FROM projects p JOIN branches b ON b.project_id=p.id WHERE p.id='${queued.project_id}' AND b.id='${queued.branch_id}'`,
    ),
  );
  if (
    !/^[a-f0-9]{32}$/.test(identity.tenant_id) ||
    !/^[a-f0-9]{32}$/.test(identity.timeline_id)
  )
    throw new Error("Exact original native storage identity required");
  const value = JSON.parse(
    kube([
      "get",
      "--raw",
      `/api/v1/namespaces/neon/services/http:pageserver-managed:9898/proxy/v1/tenant/${identity.tenant_id}/timeline/${identity.timeline_id}`,
    ]).stdout,
  );
  if (value.timeline_id !== identity.timeline_id)
    throw new Error("Original native timeline response mismatch");
  return {
    ...identity,
    ancestor_timeline_id: value.ancestor_timeline_id ?? null,
    ancestor_lsn: value.ancestor_lsn ?? null,
  };
}
function ack(name, value) {
  const file = path.join(fixture, name);
  fs.writeFileSync(file, JSON.stringify(value) + "\n", {
    flag: "wx",
    mode: 0o600,
  });
  if (process.getuid() === 0) fs.chownSync(file, owner.uid, owner.gid);
}
async function marker(name, seconds = 240) {
  const deadline = Date.now() + seconds * 1000;
  while (Date.now() < deadline && !aborted) {
    try {
      const file = path.join(fixture, name),
        stat = fs.lstatSync(file);
      if (
        !stat.isFile() ||
        stat.isSymbolicLink() ||
        stat.mode & 0o077 ||
        stat.size > 4096
      )
        throw new Error("Unsafe marker");
      return JSON.parse(fs.readFileSync(file, "utf8"));
    } catch (e) {
      if (e.code !== "ENOENT") throw e;
    }
    await delay(500);
  }
  throw new Error("Operator marker deadline or interruption");
}
function scale(replicas) {
  const d = read("deployment", "neon-control-worker");
  if (
    d.metadata.uid !== workerUID ||
    d.metadata.labels?.["app.kubernetes.io/instance"] !==
      "neon-control-plane" ||
    d.spec.template.spec.containers.length !== 1
  )
    throw new Error("Original Worker identity changed");
  kube([
    "patch",
    "deployment",
    "neon-control-worker",
    "--type=json",
    "-p",
    JSON.stringify([
      { op: "test", path: "/metadata/uid", value: workerUID },
      {
        op: "test",
        path: "/metadata/resourceVersion",
        value: d.metadata.resourceVersion,
      },
      { op: "replace", path: "/spec/replicas", value: replicas },
    ]),
  ]);
}
function retire(record) {
  if (!record) return;
  const current = kube([
    "get",
    record.resource,
    record.name,
    "--ignore-not-found",
    "-o",
    "json",
  ]).stdout.trim();
  if (!current) return;
  const value = JSON.parse(current);
  const contains = (actual, expected) =>
    Array.isArray(expected)
      ? Array.isArray(actual) &&
        actual.length === expected.length &&
        expected.every((x, i) => contains(actual[i], x))
      : expected && typeof expected === "object"
        ? actual &&
          Object.entries(expected).every(([k, v]) => contains(actual[k], v))
        : actual === expected;
  if (
    (record.uid && value.metadata.uid !== record.uid) ||
    value.metadata.labels?.["neon-control/endpoint-id"] !== report.endpointID ||
    value.metadata.labels?.["neon-control/project-id"] !== targetProject ||
    !contains(value.spec, record.expectedSpec)
  )
    throw new Error("Admission fixture identity changed; preserve for review");
  // Creation may have committed despite a lost reply. Adopt only this exact
  // owned definition after reading its identity, never repeat CREATE blindly.
  record.uid = value.metadata.uid;
  save();
  const options = {
    apiVersion: "v1",
    kind: "DeleteOptions",
    preconditions: {
      uid: record.uid,
      resourceVersion: value.metadata.resourceVersion,
    },
  };
  kube(
    [
      "delete",
      "--raw=/apis/admissionregistration.k8s.io/v1/" +
        record.resource +
        "/" +
        record.name,
      "-f",
      "-",
    ],
    { input: JSON.stringify(options) },
  );
}
save();
try {
  kube(["get", "--raw=/apis/admissionregistration.k8s.io/v1"]);
  const prepared = await marker("failed-endpoint-prepare.json");
  for (const key of ["project_id", "branch_id", "writer_id"])
    if (prepared[key] !== scope[key]) throw new Error("Original scope changed");
  if (
    sql(
      `SELECT count(*) FROM projects p JOIN branches b ON b.project_id=p.id JOIN endpoints e ON e.branch_id=b.id WHERE p.id='${scope.project_id}' AND p.source='managed' AND p.state='ready' AND b.id='${scope.branch_id}' AND b.state='ready' AND e.id='${scope.writer_id}' AND e.state='active' AND e.deleted_at IS NULL`,
    ) !== "1"
  )
    throw new Error("Original suspended Writer and ready parents required");
  const resume = scope.failed_endpoint_resume;
  if (
    sql(
      "SELECT count(*) FROM schema_migrations WHERE version=" +
        (parentCleanup ? 41 : 40),
    ) !== "1" ||
    sql(
      "SELECT to_regclass('failed_endpoint_resource_bindings') IS NOT NULL",
    ) !== "t"
  )
    throw new Error(
      "Deploy the failed endpoint cleanup migration before injecting its fault",
    );
  if (
    (!resume &&
      sql(
        "SELECT count(*) FROM operations WHERE state IN ('queued','running','retry_wait')",
      ) !== "0") ||
    sql(
      "SELECT count(*) FROM function_triggers WHERE enabled AND state='ready'",
    ) !== "0"
  )
    throw new Error(
      "Drain Operations and active schedules before this isolated fault",
    );
  if (
    JSON.parse(
      kube(["get", "virtualmachines.vm.neon.tech", "-o", "json"]).stdout,
    ).items.length !== 0
  )
    throw new Error("No VM may be running before the isolated fault");
  const d = read("deployment", "neon-control-worker");
  if (d.spec.replicas !== 1 || d.status.readyReplicas !== 1)
    throw new Error("One ready Worker required");
  workerUID = d.metadata.uid;
  report.workerUID = workerUID;
  let queued, match;
  if (resume) {
    if (
      !/^ep_[a-f0-9]{16}$/.test(resume.endpoint_id) ||
      !/^op_[a-f0-9]{24}$/.test(resume.original_create_operation_id) ||
      !resume.checks?.includes("original_native_create_failed_guard_removed") ||
      !/^publication-ui-[0-9]{14}$/.test(resume.original_failure_job)
    )
      throw new Error("Reviewed original failure continuation required");
    queued = {
      project_id: parentCleanup ? resume.project_id : scope.project_id,
      branch_id: parentCleanup ? resume.branch_id : scope.branch_id,
      endpoint_id: resume.endpoint_id,
      operation_id: resume.original_create_operation_id,
    };
    if (
      !/^prj_[a-f0-9]{16}$/.test(queued.project_id) ||
      !/^br_[a-f0-9]{16}$/.test(queued.branch_id)
    )
      throw new Error("Reviewed original parent identity required");
    targetProject = queued.project_id;
    report.endpointID = queued.endpoint_id;
    report.originalCreateOperationID = queued.operation_id;
    report.originalFailureJob = resume.original_failure_job;
    match = creationMatch(queued);
    if (
      sql(
        `SELECT count(*) FROM operations o JOIN endpoints e ON e.project_id=o.project_id WHERE ${match} AND o.state='failed' AND o.finished_at IS NOT NULL AND o.lease_owner IS NULL AND e.state IN ('error','deleting','deleted')`,
      ) !== "1"
    )
      throw new Error("Original terminal creation receipt changed");
    const priorName = "neon-ci-failed-" + queued.endpoint_id.slice(3);
    for (const kind of [
      "validatingadmissionpolicies",
      "validatingadmissionpolicybindings",
    ])
      if (
        kube([
          "get",
          kind,
          priorName,
          "--ignore-not-found",
          "-o",
          "json",
        ]).stdout.trim()
      )
        throw new Error(
          "Original guard remains; inspect its original UID before continuation",
        );
    if (
      !resume.original_service_uid ||
      !Array.isArray(resume.retained_secret_identities) ||
      resume.retained_secret_identities.length !== 2 ||
      resume.native_dependency_http_status !== 403
    )
      throw new Error("Original native operator Service/Secret proof required");
    report.originalServiceUID = resume.original_service_uid;
    report.retainedSecretIdentities = resume.retained_secret_identities;
    if (parentCleanup) {
      report.retainedTimeline = resume.retained_timeline;
      if (
        !report.retainedTimeline ||
        JSON.stringify(retainedTimeline(queued)) !==
          JSON.stringify(report.retainedTimeline)
      )
        throw new Error("Reviewed native storage identity required");
    }
    if (
      JSON.stringify(secretIdentities(queued.endpoint_id)) !==
      JSON.stringify(report.retainedSecretIdentities)
    )
      throw new Error("Original retained Secret changed before continuation");
    report.steps.push(
      "reviewed_original_native_failure_reused_no_new_create_or_policy",
    );
    save();
    ack("failed-endpoint-original-failed.json", {
      ...queued,
      native_admission_fault_removed: true,
    });
  } else {
    paused = true;
    scale(0);
    kube(
      [
        "wait",
        "--for=delete",
        "pods",
        "-l",
        "app.kubernetes.io/instance=neon-control-plane,app.kubernetes.io/component=worker",
        "--timeout=120s",
      ],
      { timeout: 150000 },
    );
    report.steps.push("original_worker_paused");
    save();
    ack("failed-endpoint-worker-paused.json", scope);
    queued = await marker("failed-endpoint-queued.json");
    if (
      !/^prj_[a-f0-9]{16}$/.test(queued.project_id) ||
      !/^br_[a-f0-9]{16}$/.test(queued.branch_id) ||
      (resourceKind !== "project" && queued.project_id !== scope.project_id) ||
      (resourceKind === "endpoint" && queued.branch_id !== scope.branch_id) ||
      (resourceKind === "project" && queued.project_id === scope.project_id) ||
      (parentCleanup && queued.branch_id === scope.branch_id) ||
      !/^ep_[a-f0-9]{16}$/.test(queued.endpoint_id) ||
      !/^op_[a-f0-9]{24}$/.test(queued.operation_id) ||
      queued.endpoint_id === scope.writer_id
    )
      throw new Error("New original endpoint/Operation required");
    report.endpointID = queued.endpoint_id;
    report.projectID = queued.project_id;
    report.branchID = queued.branch_id;
    targetProject = queued.project_id;
    report.originalCreateOperationID = queued.operation_id;
    save();
    match = creationMatch(queued);
    if (
      sql(
        `SELECT count(*) FROM operations o JOIN endpoints e ON e.project_id=o.project_id WHERE ${match} AND o.state='queued' AND o.lease_owner IS NULL AND e.state='provisioning'`,
      ) !== "1"
    )
      throw new Error("Original queued independent Reader required");
    // Dry-run a copy of the retained Writer template. This never starts a VM,
    // invokes SQL or stores the template/route Secret in evidence.
    const routes = read("secret", "neon-control-routes");
    const route = JSON.parse(
      Buffer.from(routes.data["routes.json"], "base64").toString("utf8"),
    )["ep-" + scope.writer_id.slice(3)];
    if (
      route?.project_id !== scope.project_id ||
      route.branch_id !== scope.branch_id ||
      !route.vm_template
    )
      throw new Error("Owned retained dry-run template required");
    const vm = structuredClone(route.vm_template),
      name = "cp-" + queued.endpoint_id.slice(3);
    vm.metadata.name = name;
    vm.metadata.namespace = "neon";
    vm.metadata.labels["neon-control/endpoint-id"] = queued.endpoint_id;
    vm.metadata.labels["neon-control/project-id"] = targetProject;
    const dry = (server) =>
      kube(["create", "--dry-run=server", "-f", "-", "-o", "json"], {
        input: JSON.stringify(vm),
        check: false,
        server,
      });
    if (apiServers.some((server) => dry(server).status !== 0))
      throw new Error(
        "Baseline template dry-run did not pass on every API server",
      );
    const labels = {
        "neon-control/project-id": targetProject,
        "neon-control/endpoint-id": queued.endpoint_id,
      },
      guardName = "neon-ci-failed-" + queued.endpoint_id.slice(3),
      message = "Owned CI independent endpoint create rejected";
    const definition = {
      apiVersion: "admissionregistration.k8s.io/v1",
      kind: "ValidatingAdmissionPolicy",
      metadata: { name: guardName, labels },
      spec: {
        failurePolicy: "Fail",
        matchConstraints: {
          resourceRules: [
            {
              apiGroups: ["vm.neon.tech"],
              apiVersions: ["v1"],
              operations: ["CREATE"],
              resources: ["virtualmachines"],
              scope: "Namespaced",
            },
          ],
          objectSelector: { matchLabels: labels },
        },
        matchConditions: [
          {
            name: "exact-owned-vm",
            expression: `request.namespace == 'neon' && object.metadata.name == '${name}'`,
          },
        ],
        validations: [{ expression: "false", message, reason: "Forbidden" }],
      },
    };
    policy = {
      resource: "validatingadmissionpolicies",
      name: guardName,
      uid: null,
      expectedSpec: definition.spec,
    };
    report.policy = policy;
    save();
    const created = JSON.parse(
      kube(["create", "-f", "-", "-o", "json"], {
        input: JSON.stringify(definition),
      }).stdout,
    );
    policy.uid = created.metadata.uid;
    save();
    const bindingDefinition = {
      apiVersion: "admissionregistration.k8s.io/v1",
      kind: "ValidatingAdmissionPolicyBinding",
      metadata: { name: guardName, labels },
      spec: { policyName: guardName, validationActions: ["Deny"] },
    };
    binding = {
      resource: "validatingadmissionpolicybindings",
      name: guardName,
      uid: null,
      expectedSpec: bindingDefinition.spec,
    };
    report.binding = binding;
    save();
    const bound = JSON.parse(
      kube(["create", "-f", "-", "-o", "json"], {
        input: JSON.stringify(bindingDefinition),
      }).stdout,
    );
    binding.uid = bound.metadata.uid;
    save();
    let denied = false;
    for (let i = 0; i < 40 && !aborted; i++) {
      const results = apiServers.map((server) => dry(server));
      if (
        results.every(
          (result) =>
            result.status !== 0 &&
            (result.stderr + result.stdout).includes(message),
        )
      ) {
        denied = true;
        break;
      }
      if (
        results.some(
          (result) =>
            result.status !== 0 &&
            !(result.stderr + result.stdout).includes(message),
        )
      )
        throw new Error("Unexpected dry-run rejection");
      await delay(500);
    }
    if (!denied) throw new Error("Owned admission fault did not activate");
    report.steps.push("baseline_dry_run_passed_and_exact_vm_denial_proven");
    save();
    scale(1);
    paused = false;
    kube(
      ["rollout", "status", "deployment/neon-control-worker", "--timeout=120s"],
      { timeout: 150000 },
    );
    let failed = false;
    for (let i = 0; i < 120 && !aborted; i++) {
      if (
        sql(
          `SELECT count(*) FROM operations o JOIN endpoints e ON e.project_id=o.project_id WHERE ${match} AND o.state='failed' AND o.finished_at IS NOT NULL AND o.lease_owner IS NULL AND e.state='error'`,
        ) === "1"
      ) {
        failed = true;
        break;
      }
      await delay(1000);
    }
    if (!failed) throw new Error("Original native create failure not observed");
    const dependency = kube([
      "logs",
      "deployment/neon-control-worker",
      "--tail=200",
    ])
      .stdout.split("\n")
      .flatMap((line) => {
        try {
          return [JSON.parse(line)];
        } catch {
          return [];
        }
      })
      .find(
        (value) =>
          value.operation_id === queued.operation_id &&
          value.msg === "operation dependency failure" &&
          value.http_status === 403,
      );
    if (!dependency)
      throw new Error("Original structured dependency HTTP 403 proof missing");
    const service = read("service", name);
    if (
      service.metadata.labels?.["neon-control/project-id"] !== targetProject ||
      service.metadata.labels?.["neon-control/endpoint-id"] !==
        queued.endpoint_id ||
      service.spec.selector?.["vm.neon.tech/name"] !== name ||
      !service.metadata.uid
    )
      throw new Error("Original pre-VM Service identity missing");
    if (
      kube([
        "get",
        "virtualmachine.vm.neon.tech",
        name,
        "--ignore-not-found",
        "-o",
        "json",
      ]).stdout.trim()
    )
      throw new Error("Admission fault unexpectedly created a VM");
    report.nativeDependencyHTTPStatus = dependency.http_status;
    report.originalServiceUID = service.metadata.uid;
    report.retainedSecretIdentities = secretIdentities(queued.endpoint_id);
    save();
    retire(binding);
    retire(policy);
    binding = null;
    policy = null;
    let restored = false;
    for (let i = 0; i < 40 && !aborted; i++) {
      if (apiServers.every((server) => dry(server).status === 0)) {
        restored = true;
        break;
      }
      await delay(500);
    }
    if (!restored)
      throw new Error("Owned admission guard removal not observed");
    report.steps.push("native_original_create_failed_and_guard_retired");
    if (parentCleanup) report.retainedTimeline = retainedTimeline(queued);
    save();
    ack("failed-endpoint-original-failed.json", {
      ...queued,
      native_admission_fault_removed: true,
    });
  }
  const done = await marker("failed-endpoint-cleanup-finished.json");
  if (
    done.endpoint_id !== queued.endpoint_id ||
    !/^op_[a-f0-9]{24}$/.test(done.cleanup_operation_id)
  )
    throw new Error("Original cleanup identity mismatch");
  if (
    sql(
      `SELECT count(*) FROM operations o JOIN endpoints e ON e.project_id=o.project_id WHERE ${match} AND o.state='failed' AND e.state='deleted' AND e.deletion_operation_id='${done.cleanup_operation_id}'`,
    ) !== "1"
  )
    throw new Error("Original failure or endpoint tombstone not retained");
  const witnesses = JSON.parse(
    sql(
      `SELECT COALESCE(jsonb_object_agg(b.resource_kind,b.observed_uid),'{}') FROM failed_endpoint_resource_bindings b JOIN operations o ON o.id=b.operation_id WHERE b.operation_id='${done.cleanup_operation_id}' AND b.endpoint_id='${queued.endpoint_id}' AND b.project_id='${targetProject}' AND o.action='cleanup_failed_${resourceKind}' AND o.state='succeeded'`,
    ),
  );
  if (
    Object.keys(witnesses).length !== 2 ||
    witnesses.vm !== "" ||
    witnesses.service !== report.originalServiceUID
  )
    throw new Error("Original VM/Service witness mismatch");
  if (
    kube([
      "get",
      "service",
      "cp-" + queued.endpoint_id.slice(3),
      "--ignore-not-found",
      "-o",
      "json",
    ]).stdout.trim()
  )
    throw new Error("Original endpoint Service remains");
  if (
    JSON.stringify(secretIdentities(queued.endpoint_id)) !==
    JSON.stringify(report.retainedSecretIdentities)
  )
    throw new Error("Original credentials or config Secret changed");
  report.cleanupOperationID = done.cleanup_operation_id;
  if (
    parentCleanup &&
    JSON.stringify(retainedTimeline(queued)) !==
      JSON.stringify(report.retainedTimeline)
  )
    throw new Error(
      "Original native timeline or ancestry changed during cleanup",
    );
  report.steps.push(
    "original_vm_service_witnesses_retirement_and_secret_preservation_verified",
  );
  report.result = "pass";
} catch (e) {
  report.result = "fail";
  report.error = e.message;
  process.exitCode = 1;
} finally {
  clearInterval(abortWatch);
  try {
    retire(binding);
    retire(policy);
    report.ownedPoliciesRetired = true;
  } catch (e) {
    report.guardRetirementFailed = e.message;
    report.result = "fail";
    process.exitCode = 1;
  }
  if (paused) {
    try {
      scale(1);
      kube(
        [
          "rollout",
          "status",
          "deployment/neon-control-worker",
          "--timeout=120s",
        ],
        { timeout: 150000 },
      );
      report.workerRestoredAfterFailure = true;
    } catch (e) {
      report.workerRestorationFailed = e.message;
      report.result = "fail";
      process.exitCode = 1;
    }
  }
  if (workerUID) {
    try {
      const d = read("deployment", "neon-control-worker");
      if (d.metadata.uid !== workerUID || d.spec.replicas !== 1)
        throw new Error("Worker restore identity mismatch");
      report.originalWorkerRestored = true;
    } catch (e) {
      report.workerRestorationFailed = e.message;
      report.result = "fail";
      process.exitCode = 1;
    }
  }
  save();
}
