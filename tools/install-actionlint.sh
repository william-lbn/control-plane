#!/usr/bin/env bash
set -euo pipefail
task_dir="${1:?Destination directory required}"
task_version=1.7.12
task_sha=8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8
mkdir -p "$task_dir"
task_archive="$task_dir/actionlint-$task_version-linux-amd64.tar.gz"
curl --fail --location --retry 2 --proto '=https' --tlsv1.2 \
  "https://github.com/rhysd/actionlint/releases/download/v$task_version/actionlint_${task_version}_linux_amd64.tar.gz" \
  -o "$task_archive"
printf '%s  %s\n' "$task_sha" "$task_archive" | sha256sum --check --strict
tar -xzf "$task_archive" -C "$task_dir" actionlint
"$task_dir/actionlint" -version
