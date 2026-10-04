#!/usr/bin/env bash
set -euo pipefail
task_destination="${1:?Destination directory required}"
task_version="$(node -p "require('./containers/builders.lock.json').helm.version")"
task_digest="$(node -p "require('./containers/builders.lock.json').helm.linux_amd64_sha256")"
task_temp="$(mktemp -d)"
trap 'rm -rf -- "$task_temp"' EXIT
curl --fail --location --proto '=https' --tlsv1.2 "https://get.helm.sh/helm-v$task_version-linux-amd64.tar.gz" -o "$task_temp/helm.tar.gz"
printf '%s  %s\n' "$task_digest" "$task_temp/helm.tar.gz" | sha256sum --check --status
tar -xzf "$task_temp/helm.tar.gz" -C "$task_temp" linux-amd64/helm
install -d "$task_destination"
install -m 0755 "$task_temp/linux-amd64/helm" "$task_destination/helm"
