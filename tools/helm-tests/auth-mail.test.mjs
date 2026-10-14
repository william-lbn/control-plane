import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import test from "node:test";
import { documents } from "../check-helm-rbac.mjs";
const base = [
  "template",
  "test",
  "charts/neon-control-plane",
  "--namespace",
  "neon",
  "-f",
  "charts/neon-control-plane/ci/render-values.yaml",
];
const auth = [
  "--set",
  "managedAuth.enabled=true",
  "--set",
  "managedAuth.labHTTP=true",
  "--set",
  "managedAuth.publicOrigin=http://192.0.2.1:30788",
  "--set",
  "managedAuth.pgCASecret=proxy-ca",
  "--set",
  "managedAuth.pgServerName=proxy.example.test",
  "--set",
  "managedAuth.runtimeImage=example.invalid/auth@sha256:" + "c".repeat(64),
];
const mail = [
  "--set",
  "managedAuth.mail.enabled=true",
  "--set",
  "managedAuth.mail.existingSecret=operator-smtp",
  "--set",
  "managedAuth.labMailbox.enabled=true",
  "--set",
  "managedAuth.labMailbox.acknowledged=true",
  "--set",
  "managedAuth.labMailbox.existingAuthSecret=independent-inbox-reader",
];
test("mail fixture retains data, restricts access and separates reader credentials", () => {
  const objects = documents(
    execFileSync("helm", [...base, ...auth, ...mail], { encoding: "utf8" }),
  );
  const receiver = objects.find(
    (o) => o.kind === "Deployment" && o.metadata.name.endsWith("-auth-mailbox"),
  ).spec.template.spec;
  assert.equal(receiver.automountServiceAccountToken, false);
  assert.equal(
    receiver.containers[0].securityContext.readOnlyRootFilesystem,
    true,
  );
  assert.equal(
    receiver.volumes.find((v) => v.name === "auth").secret.secretName,
    "independent-inbox-reader",
  );
  assert.equal(
    receiver.containers[0].env.find((e) => e.name === "MP_MAX_MESSAGES").value,
    "0",
  );
  const service = objects.find(
    (o) => o.kind === "Service" && o.metadata.name === "neon-auth-mailbox",
  );
  assert.equal(service.spec.type, "ClusterIP");
  assert.equal(
    service.spec.ports.some((p) => p.nodePort),
    false,
  );
  assert.equal(
    objects.find(
      (o) =>
        o.kind === "PersistentVolumeClaim" &&
        o.metadata.name.endsWith("-auth-mailbox"),
    ).metadata.annotations["helm.sh/resource-policy"],
    "keep",
  );
  const policy = objects.find(
    (o) =>
      o.kind === "NetworkPolicy" && o.metadata.name.endsWith("-auth-mailbox"),
  );
  assert.deepEqual(policy.spec.egress, []);
  assert.equal(
    policy.spec.ingress[1].from[0].podSelector.matchLabels[
      "neon-control/test-fixture"
    ],
    "auth-mail",
  );
  for (const object of objects.filter(
    (o) =>
      o.kind === "Deployment" && !o.metadata.name.endsWith("-auth-mailbox"),
  )) {
    const pod = object.spec.template.spec;
    assert.equal(
      (pod.volumes ?? []).some(
        (v) => v.secret?.secretName === "independent-inbox-reader",
      ),
      false,
    );
    if (
      object.metadata.name.endsWith("-api") ||
      object.metadata.name.endsWith("-worker")
    )
      assert.equal(
        pod.containers[0].env.find((e) => e.name === "NEON_AUTH_MAIL_SECRET")
          .value,
        "operator-smtp",
      );
  }
});
test("mail is disabled by default and unsafe fixture input is rejected", () => {
  assert.equal(
    documents(execFileSync("helm", base, { encoding: "utf8" })).some(
      (o) =>
        o.kind === "Deployment" && o.metadata.name.endsWith("-auth-mailbox"),
    ),
    false,
  );
  for (const overrides of [
    ["--set", "managedAuth.mail.existingSecret="],
    ["--set", "managedAuth.labMailbox.acknowledged=false"],
    ["--set", "managedAuth.labMailbox.existingAuthSecret="],
    ["--set", "managedAuth.labMailbox.image=example.invalid/mail:latest"],
  ])
    assert.throws(() =>
      execFileSync("helm", [...base, ...auth, ...mail, ...overrides], {
        stdio: "pipe",
      }),
    );
});
