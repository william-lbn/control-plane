#!/usr/bin/env bash
# Offline supervisor-only update. This is deliberately labelled a derived guest,
# not a rebuild of the parent rootfs/packages/kernel from new upstream source.
# Dependencies: buildctl (Linux worker), debugfs, sha256sum, node, tar.
set -euo pipefail
[[ $# == 6 || $# == 7 ]] || { echo 'Usage: derive-functions-vm.sh PARENT@sha256 RUNNER@sha256 SUPERVISOR SOURCE_SHA256 OUTPUT BUILDCTL_ADDRESS [REVIEWED_RUNTIME_TREE]'; exit 2; }
task_parent="$1"; task_runner="$2"; task_supervisor="$(realpath "$3")"; task_source="$4"; task_output="$5"; task_address="$6"
task_runtime="${7:-}"
[[ "$task_parent" =~ ^[a-z0-9./-]+@sha256:[a-f0-9]{64}$ && "$task_runner" =~ ^[a-z0-9./-]+@sha256:[a-f0-9]{64}$ && "$task_source" =~ ^[a-f0-9]{64}$ ]] || { echo 'Pinned inputs required'; exit 2; }
[[ -s "$task_supervisor" && ! -e "$task_output" ]] || { echo 'New output and compiled supervisor required'; exit 2; }
mkdir -m 0700 -p "$task_output"
task_output="$(realpath "$task_output")"
command -v debugfs >/dev/null; command -v buildctl >/dev/null; command -v node >/dev/null
task_tool_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
task_go_builder="${FUNCTIONS_IMAGE_TOOL_GO_BUILDER:-$(node -e 'process.stdout.write(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).images.go)' "$task_tool_dir/../containers/builders.lock.json")}"
[[ "$task_go_builder" =~ ^[a-z0-9./_-]+@sha256:[a-f0-9]{64}$ && -f "$task_tool_dir/expand-function-image/main.go" ]] || { echo 'Pinned sparse build helper inputs required'; exit 2; }
mkdir -m 0700 -p "$task_output/expand-tool"
cp "$task_tool_dir/expand-function-image/main.go" "$task_tool_dir/expand-function-image/main_test.go" "$task_output/expand-tool/"
task_parent_from="$task_parent"; task_context_args=()
if [[ -n "${FUNCTIONS_PARENT_OCI_LAYOUT:-}" ]]; then
 [[ "$FUNCTIONS_PARENT_OCI_LAYOUT" =~ ^/[a-zA-Z0-9/_.-]+$ && -f "$FUNCTIONS_PARENT_OCI_LAYOUT/index.json" ]] || exit 2
 task_parent_from=parent_input
 task_context_args=(--oci-layout "parent_input=$FUNCTIONS_PARENT_OCI_LAYOUT" --opt "context:parent_input=oci-layout://parent_input@${task_parent##*@}")
fi
cp "$task_supervisor" "$task_output/function-supervisor"
chmod 0755 "$task_output/function-supervisor"
cat > "$task_output/Dockerfile.convert" <<'DOCKER'
ARG PARENT
ARG RUNNER
FROM ${PARENT} AS parent
FROM ${RUNNER} AS convert
USER root
COPY --from=parent /disk.qcow2 /work/parent.qcow2
RUN qemu-img info --output=json /work/parent.qcow2 > /work/parent-info.json && \
    qemu-img convert -f qcow2 -O raw /work/parent.qcow2 /work/root.raw && \
    gzip -c -1 /work/root.raw > /work/root.raw.gz
FROM scratch
COPY --from=convert /work/root.raw.gz /root.raw.gz
COPY --from=convert /work/parent-info.json /parent-info.json
DOCKER
buildctl --addr "$task_address" build --progress=plain --frontend=dockerfile.v0 \
 --local "context=$task_output" --local "dockerfile=$task_output" --opt filename=Dockerfile.convert \
 --opt "build-arg:PARENT=$task_parent_from" --opt "build-arg:RUNNER=$task_runner" "${task_context_args[@]}" --output "type=local,dest=$task_output/raw"
gzip -dc "$task_output/raw/root.raw.gz" | dd of="$task_output/raw/root.raw" bs=1M conv=sparse status=none
# Modify a new offline copy only. debugfs status alone is insufficient; extract
# the replacement and compare bytes/mode before producing any bootable carrier.
debugfs -w -R 'rm /usr/local/bin/function-supervisor' "$task_output/raw/root.raw"
debugfs -w -R "write $task_output/function-supervisor /usr/local/bin/function-supervisor" "$task_output/raw/root.raw"
debugfs -w -R 'set_inode_field /usr/local/bin/function-supervisor mode 0100755' "$task_output/raw/root.raw"
debugfs -R "dump /usr/local/bin/function-supervisor $task_output/supervisor.actual" "$task_output/raw/root.raw"
cmp "$task_output/function-supervisor" "$task_output/supervisor.actual"
debugfs -R 'stat /usr/local/bin/function-supervisor' "$task_output/raw/root.raw" > "$task_output/supervisor.stat"
grep -Eq 'Mode: +0755' "$task_output/supervisor.stat"
if [[ -n "$task_runtime" ]]; then
 node "$(dirname "$0")/install-derived-function-runtime.mjs" "$task_runtime" "$task_output/raw/root.raw" "$task_output/runtime-verification"
fi
# Transfer compressed immutable bytes to BuildKit instead of a mostly empty
# 2 GiB raw filesystem. The seal stage expands the exact verified offline image.
gzip -c -1 "$task_output/raw/root.raw" > "$task_output/root.raw.gz"
PARENT_IMAGE="$task_parent" RUNNER_IMAGE="$task_runner" SOURCE_HASH="$task_source" OUTPUT="$task_output" RUNTIME_TREE="$task_runtime" node --input-type=module - <<'NODE'
import fs from 'node:fs';import crypto from 'node:crypto';
const p=process.env.OUTPUT;const hash=f=>crypto.createHash('sha256').update(fs.readFileSync(f)).digest('hex');
const info=JSON.parse(fs.readFileSync(p+'/raw/parent-info.json'));
if(info.format!=='qcow2'||info['virtual-size']!==2147483648||info['backing-filename'])throw Error('Standalone 2 GiB parent required');
const runtime=process.env.RUNTIME_TREE ? JSON.parse(fs.readFileSync(p+'/runtime-verification/result.json')) : null;
fs.writeFileSync(p+'/functions-derived-inputs.json',JSON.stringify({kind:runtime?'derived-supervisor-runtime-v1':'derived-supervisor-only-v1',parent_image:process.env.PARENT_IMAGE,tools_image:process.env.RUNNER_IMAGE,source_snapshot_sha256:process.env.SOURCE_HASH,supervisor_sha256:hash(p+'/function-supervisor'),parent_virtual_bytes:info['virtual-size'],parent_packages_retained:true,node_binary_retained:true,node_runtime_retained:!runtime,runtime},null,2)+'\n');
NODE
cat > "$task_output/Dockerfile.carrier" <<'DOCKER'
ARG PARENT
ARG RUNNER
ARG GO_BUILDER_IMAGE
FROM ${GO_BUILDER_IMAGE} AS sparse
WORKDIR /expand
COPY expand-tool ./
RUN --mount=type=cache,id=neon-control-gobuild-1-27,target=/root/.cache/go-build GO111MODULE=off GOTOOLCHAIN=local GOMAXPROCS=2 go test -v . && CGO_ENABLED=0 GO111MODULE=off GOTOOLCHAIN=local go build -trimpath -ldflags='-s -w' -o /expand-function-image .
FROM ${RUNNER} AS seal
USER root
COPY root.raw.gz /work/root.raw.gz
COPY --from=sparse /expand-function-image /usr/local/bin/expand-function-image
RUN /usr/local/bin/expand-function-image /work/root.raw.gz /work/root.raw 2147483648 && e2fsck -fn /work/root.raw && qemu-img convert -f raw -O qcow2 -o cluster_size=2M,lazy_refcounts=on /work/root.raw /work/disk.qcow2 && qemu-img check /work/disk.qcow2
FROM ${PARENT}
USER root
COPY --from=seal /work/disk.qcow2 /disk.qcow2
COPY functions-derived-inputs.json /functions-derived-inputs.json
ENTRYPOINT []
CMD ["/bin/true"]
DOCKER
buildctl --addr "$task_address" build --progress=plain --frontend=dockerfile.v0 \
 --local "context=$task_output" --local "dockerfile=$task_output" --opt filename=Dockerfile.carrier \
 --opt "build-arg:PARENT=$task_parent_from" --opt "build-arg:RUNNER=$task_runner" --opt "build-arg:GO_BUILDER_IMAGE=$task_go_builder" "${task_context_args[@]}" \
 --output "type=oci,name=neon.local/control-functions-vm:derived-$task_source,dest=$task_output/functions-vm.oci.tar" \
 --metadata-file "$task_output/build-metadata.json"
sha256sum "$task_output/functions-vm.oci.tar" "$task_output/function-supervisor" "$task_output/functions-derived-inputs.json" > "$task_output/SHA256SUMS"
echo 'Derived guest built. Preserve receipts; import and pin its manifest digest before changing operator configuration.'
