#!/usr/bin/env bash
set -euo pipefail
task_destination="${1:?Destination directory required}"
task_url="$(node -p "require('./containers/services.lock.json').postgrest.linux_amd64_url")"
task_digest="$(node -p "require('./containers/services.lock.json').postgrest.linux_amd64_sha256")"
task_temp="$(mktemp -d)"
trap 'rm -rf -- "$task_temp"' EXIT
curl --fail --location --proto '=https' --tlsv1.2 "$task_url" -o "$task_temp/postgrest.tar.xz"
printf '%s  %s\n' "$task_digest" "$task_temp/postgrest.tar.xz" | sha256sum --check --status
tar -xJf "$task_temp/postgrest.tar.xz" -C "$task_temp" postgrest
install -d "$task_destination"
install -m 0755 "$task_temp/postgrest" "$task_destination/postgrest"
"$task_destination/postgrest" --version
