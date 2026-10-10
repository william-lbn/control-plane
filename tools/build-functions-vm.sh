#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
[[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] || { echo 'Linux amd64 builder required'; exit 1; }
: "${FUNCTIONS_SOURCE_COMMIT:?Exact reviewed source required}"
[[ "$FUNCTIONS_SOURCE_COMMIT" =~ ^[a-f0-9]{40}$ ]] || exit 1
[[ "$(git rev-parse HEAD)" == "$FUNCTIONS_SOURCE_COMMIT" ]] || { echo 'Source commit mismatch'; exit 1; }
git diff --quiet && git diff --cached --quiet || { echo 'Commit all reviewed source changes before building'; exit 1; }
[[ -z "$(git ls-files --others --exclude-standard)" ]] || { echo 'Untracked source cannot enter a revision-labeled guest'; exit 1; }
task_output="$PWD/artifacts/functions-vm"
mkdir -p "$task_output"
[[ ! -e "$task_output/disk.qcow2" ]] || { echo 'Retain existing build evidence'; exit 1; }
mapfile -t task_inputs < <(node --input-type=module - <<'NODE'
import fs from 'node:fs';
const l = JSON.parse(fs.readFileSync('containers/functions.lock.json'));
const b = JSON.parse(fs.readFileSync('containers/builders.lock.json'));
for (const v of [b.images.go, l.node_image, l.vm_builder, l.vm_daemon]) {
  if (!/^[a-z0-9./-]+@sha256:[a-f0-9]{64}$/.test(v)) throw Error('Unpinned Functions input');
  console.log(v);
}
if (!/^\d{8}T\d{6}Z$/.test(l.debian_snapshot) || l.root_disk_size !== '2G') throw Error('Invalid snapshot/disk');
console.log(l.debian_snapshot);
NODE
)
task_go="${task_inputs[0]}"; task_node="${task_inputs[1]}"
task_builder="${task_inputs[2]}"; task_daemon="${task_inputs[3]}"
task_root="neon-functions-rootfs:$FUNCTIONS_SOURCE_COMMIT"
task_intermediate="neon-functions-unsealed:$FUNCTIONS_SOURCE_COMMIT"
docker build --platform linux/amd64 -f Dockerfile.functions-rootfs \
  --build-arg "GO_BUILDER_IMAGE=$task_go" --build-arg "NODE_BUILDER_IMAGE=$task_node" \
  --build-arg "DEBIAN_SNAPSHOT=${task_inputs[4]}" -t "$task_root" .
docker run --rm --entrypoint /bin/cat "$task_root" /opt/neon/functions/guest-packages.txt > "$task_output/guest-packages.txt"
# Builder, daemon and target kernel are the existing own-fork image lock. Docker
# daemon access is confined to this disposable CI builder, never a control pod.
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/services/functions/guest:/work:ro" "$task_builder" \
  --src "$task_root" --dst "$task_intermediate" --size 2G \
  --spec /work/image-spec.yaml --daemon-image "$task_daemon" --target-arch linux/amd64
task_container="$(docker create "$task_intermediate")"
trap 'docker rm "$task_container" >/dev/null' EXIT
docker cp "$task_container:/disk.qcow2" "$task_output/unsealed.qcow2"
qemu-img convert -f qcow2 -O raw "$task_output/unsealed.qcow2" "$task_output/root.raw"
# Specialize the generated image offline, without modifying the upstream fork.
# debugfs can return zero on command errors: compare actual disk contents below.
debugfs -w -R 'rm /etc/inittab' "$task_output/root.raw"
debugfs -w -R "write $PWD/services/functions/guest/inittab /etc/inittab" "$task_output/root.raw"
debugfs -w -R 'set_inode_field /etc/inittab mode 0100644' "$task_output/root.raw"
debugfs -R 'cat /etc/inittab' "$task_output/root.raw" > "$task_output/inittab.actual"
cmp services/functions/guest/inittab "$task_output/inittab.actual"
e2fsck -fn "$task_output/root.raw"
qemu-img convert -f raw -O qcow2 -o cluster_size=2M,lazy_refcounts=on "$task_output/root.raw" "$task_output/disk.qcow2"
qemu-img info --output=json "$task_output/disk.qcow2" > "$task_output/disk-info.json"
sha256sum "$task_output/disk.qcow2" "$task_output/guest-packages.txt" "$task_output/inittab.actual" > "$task_output/SHA256SUMS"
printf '%s\n' "$FUNCTIONS_SOURCE_COMMIT" > "$task_output/source-commit.txt"
mkdir "$task_output/carrier"
cp "$task_output/disk.qcow2" "$task_output/guest-packages.txt" "$task_output/carrier/"
cp containers/functions.lock.json "$task_output/carrier/functions-build-inputs.json"
# The carrier is also part of the NeonVM startup contract. Exercise the exact
# own-controller loader command in a disposable network namespace before push;
# no customer bundle is executed by this privileged, isolated CI check.
task_carrier="neon-functions-carrier-verified:$FUNCTIONS_SOURCE_COMMIT"
docker build --platform linux/amd64 -f Dockerfile.functions-vm \
  --build-arg "NODE_BUILDER_IMAGE=$task_node" \
  --build-arg "DEBIAN_SNAPSHOT=${task_inputs[4]}" -t "$task_carrier" "$task_output/carrier"
docker run --rm --privileged --network none --entrypoint /bin/sh "$task_carrier" -ec \
  'mkdir -p /vm/images; mv /disk.qcow2 /vm/images/rootdisk.qcow2 && chown 36:34 /vm/images/rootdisk.qcow2 && sysctl -w net.ipv4.ip_forward=1; test -s /vm/images/rootdisk.qcow2; stat -c %u:%g /vm/images/rootdisk.qcow2 | grep -qx 36:34' \
  > "$task_output/carrier-contract.log"
docker run --rm --entrypoint /bin/cat "$task_carrier" /carrier-packages.txt > "$task_output/carrier-packages.txt"
# Large intermediate files are CI-local and are deliberately not published.
