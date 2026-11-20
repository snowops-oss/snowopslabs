#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Registry mirrors for the local runtimes, read from REGISTRY_MIRRORS.
#
# REGISTRY_MIRRORS is a comma-separated list of <registry>=<mirror> entries:
#   docker.io=docker-mirror.example.com,ghcr.io=https://ghcr-mirror.example.com
# A mirror without a scheme is reached over https. A node asks the mirror first
# and falls back to the original registry when the mirror cannot serve an image.
#
# Sourced, not executed:
#   . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/_lib/mirrors.sh"

mirror_invalid() {
  echo "ERROR: REGISTRY_MIRRORS entry '$1' is not valid." >&2
  echo "  Write comma-separated <registry>=<mirror> entries, one per registry, for example" >&2
  echo "  REGISTRY_MIRRORS=docker.io=docker-mirror.example.com,ghcr.io=ghcr-mirror.example.com" >&2
}

# mirror_pairs prints one "<registry> <mirror-url>" line per REGISTRY_MIRRORS
# entry and prints nothing when it is unset. It returns 1 on a malformed entry.
mirror_pairs() {
  local entries=() entry registry mirror url seen=" "
  IFS=, read -r -a entries <<<"${REGISTRY_MIRRORS:-}"
  for entry in ${entries[@]+"${entries[@]}"}; do
    entry="$(printf '%s' "$entry" | tr -d '[:space:]')"
    [ -n "$entry" ] || continue
    case "$entry" in
      *=*) ;;
      *)
        mirror_invalid "$entry"
        return 1
        ;;
    esac
    registry="${entry%%=*}"
    mirror="${entry#*=}"
    case "$mirror" in
      http://* | https://*) url="$mirror" ;;
      *://*)
        mirror_invalid "$entry"
        return 1
        ;;
      *) url="https://$mirror" ;;
    esac
    case "$registry" in
      '' | *[!A-Za-z0-9.:-]*)
        mirror_invalid "$entry"
        return 1
        ;;
    esac
    case "${url#*://}" in
      '' | */ | *[!A-Za-z0-9._:/-]*)
        mirror_invalid "$entry"
        return 1
        ;;
    esac
    case "$seen" in
      *" $registry "*)
        mirror_invalid "$entry"
        return 1
        ;;
    esac
    seen="$seen$registry "
    printf '%s %s\n' "$registry" "$url"
  done
}

# k3d_registries_yaml <pairs> prints a k3s registries.yaml for the pairs
# mirror_pairs printed. `k3d cluster create --registry-config` hands it to every
# node, including agents added later.
k3d_registries_yaml() {
  local registry url
  echo "mirrors:"
  while read -r registry url; do
    [ -n "$registry" ] || continue
    printf '  "%s":\n    endpoint:\n      - "%s"\n' "$registry" "$url"
  done <<<"$1"
}

# kind_apply_mirrors <pairs> <node>... writes a containerd hosts.toml for each
# pair into each node container. containerd reads it on the next pull, so no
# restart is needed. `server` names the original registry, so a mirror that
# cannot serve an image falls back to it. The cluster must have been created
# with config_path set to /etc/containerd/certs.d.
kind_apply_mirrors() {
  local pairs="$1" node registry url server
  shift
  for node in "$@"; do
    while read -r registry url; do
      [ -n "$registry" ] || continue
      server="https://$registry"
      [ "$registry" != docker.io ] || server="https://registry-1.docker.io"
      printf 'server = "%s"\n\n[host."%s"]\n  capabilities = ["pull", "resolve"]\n' "$server" "$url" |
        docker exec -i "$node" sh -c \
          "mkdir -p /etc/containerd/certs.d/$registry && cat >/etc/containerd/certs.d/$registry/hosts.toml"
    done <<<"$pairs"
  done
}
