// Read-only Linux CNI diagnostics. VERSION never selects a namespace or makes
// ADD/DEL network changes. Use before rebuilding workloads after node reboot.
import { parseArgs } from "node:util";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
const { values } = parseArgs({
  options: {
    directory: { type: "string", default: "/opt/cni/bin" },
    output: { type: "string" },
  },
});
if (process.platform !== "linux")
  throw new Error("CNI diagnostics must run on Linux");
const directory = path.resolve(values.directory);
const plugins = [
  "bandwidth",
  "bridge",
  "dhcp",
  "dummy",
  "firewall",
  "host-device",
  "host-local",
  "ipvlan",
  "loopback",
  "macvlan",
  "portmap",
  "ptp",
  "sbr",
  "static",
  "tap",
  "tuning",
  "vlan",
  "vrf",
];
const checks = [];
for (const name of plugins) {
  const file = path.join(directory, name);
  if (!fs.existsSync(file)) continue;
  const before = fs.readFileSync(file),
    sha = createHash("sha256").update(before).digest("hex");
  let passed = false,
    supportedVersions = [],
    failure = "";
  try {
    const value = JSON.parse(
      execFileSync(file, [], {
        env: { ...process.env, CNI_COMMAND: "VERSION" },
        timeout: 5000,
        encoding: "utf8",
        stdio: ["ignore", "pipe", "pipe"],
      }),
    );
    supportedVersions = value.supportedVersions;
    passed =
      Array.isArray(supportedVersions) && supportedVersions.includes("1.0.0");
    if (!passed) failure = "unsupported_version_response";
  } catch (error) {
    failure = error.signal || "exit_" + String(error.status);
  }
  const unchanged =
    createHash("sha256").update(fs.readFileSync(file)).digest("hex") === sha;
  checks.push({
    plugin: name,
    sha256: sha,
    size_bytes: before.length,
    passed: passed && unchanged,
    supported_versions: supportedVersions,
    failure: unchanged ? failure : "file_changed_during_read",
  });
}
const report = {
  result: checks.length > 0 && checks.every((c) => c.passed) ? "pass" : "fail",
  platform: "linux",
  read_only: true,
  network_mutations: false,
  checks,
};
if (values.output)
  fs.writeFileSync(values.output, JSON.stringify(report, null, 2) + "\n", {
    flag: "wx",
    mode: 0o600,
  });
console.log(JSON.stringify(report));
if (report.result !== "pass") process.exitCode = 1;
