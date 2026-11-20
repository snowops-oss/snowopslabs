#!/usr/bin/env bats
# SPDX-License-Identifier: Apache-2.0
#
# REGISTRY_MIRRORS: a malformed value stops the lab before it builds a cluster,
# and a valid one reaches the nodes (k3d registries.yaml, kind hosts.toml) so
# images are pulled through the mirror.

load 'helpers/stub'

setup() {
  stub_setup
  stub_command k3d kind kubectl docker
  ROOT="$(project_root)"
  export REACHABLE_WAIT=0
  unset REGISTRY_MIRRORS
}

teardown() {
  stub_teardown
}

# --- mirror_pairs -----------------------------------------------------------

pairs() {
  bash -c '. "$1/runtimes/_lib/mirrors.sh" && mirror_pairs' _ "$ROOT"
}

@test "mirror_pairs prints nothing when REGISTRY_MIRRORS is unset or empty" {
  run pairs
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  REGISTRY_MIRRORS="" run pairs
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "mirror_pairs reaches a mirror over https unless it names a scheme" {
  REGISTRY_MIRRORS="docker.io=docker-mirror.example.com, ghcr.io=http://10.0.0.5:5000,quay.io=https://q.example.com/mirror" run pairs
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "docker.io https://docker-mirror.example.com" ]
  [ "${lines[1]}" = "ghcr.io http://10.0.0.5:5000" ]
  [ "${lines[2]}" = "quay.io https://q.example.com/mirror" ]
}

@test "mirror_pairs rejects a malformed entry and says how to write one" {
  local bad
  for bad in \
    "docker.io" \
    "docker.io=" \
    "=mirror.example.com" \
    "docker.io=ftp://mirror.example.com" \
    "docker.io=mirror.example.com/" \
    "docker.io=mirror.example.com/a?b" \
    "docker/io=mirror.example.com" \
    "docker.io=a.example.com,docker.io=b.example.com"; do
    REGISTRY_MIRRORS="$bad" run pairs
    [ "$status" -eq 1 ] || {
      echo "accepted: $bad" >&2
      return 1
    }
    [[ "$output" == *"REGISTRY_MIRRORS entry"* ]] || false
    [[ "$output" == *"<registry>=<mirror>"* ]] || false
  done
}

# --- k3d --------------------------------------------------------------------

# k3d_keeps_registry_config makes `cluster create` keep a copy of the file
# --registry-config names, since up.sh deletes it when it exits.
k3d_keeps_registry_config() {
  stub_script k3d <<EOF
case "\$*" in
  *"cluster list"*) exit 1 ;;
  *"cluster create"*)
    while [ \$# -gt 0 ]; do
      [ "\$1" = --registry-config ] && cp "\$2" "$STUB_DIR/registries.yaml"
      shift
    done
    ;;
esac
EOF
}

@test "k3d up gives every node the mirrors in a registries.yaml" {
  k3d_keeps_registry_config
  REGISTRY_MIRRORS="docker.io=docker-mirror.example.com,ghcr.io=http://10.0.0.5:5000" \
    run bash "$ROOT/runtimes/k3d/up.sh" testcluster
  [ "$status" -eq 0 ]
  assert_called k3d "cluster create"
  assert_called k3d "--registry-config"
  run cat "$STUB_DIR/registries.yaml"
  [ "$status" -eq 0 ]
  [[ "$output" == *'"docker.io":'* ]] || false
  [[ "$output" == *'- "https://docker-mirror.example.com"'* ]] || false
  [[ "$output" == *'"ghcr.io":'* ]] || false
  [[ "$output" == *'- "http://10.0.0.5:5000"'* ]] || false
}

@test "k3d up passes no registry config when REGISTRY_MIRRORS is unset" {
  stub_when k3d "cluster list" 1
  run bash "$ROOT/runtimes/k3d/up.sh" testcluster
  [ "$status" -eq 0 ]
  assert_called k3d "cluster create"
  refute_called k3d "--registry-config"
}

@test "k3d up stops before creating a cluster when REGISTRY_MIRRORS is malformed" {
  stub_when k3d "cluster list" 1
  REGISTRY_MIRRORS="docker.io" run bash "$ROOT/runtimes/k3d/up.sh" testcluster
  [ "$status" -eq 1 ]
  [[ "$output" == *"REGISTRY_MIRRORS entry 'docker.io'"* ]] || false
  refute_called k3d "cluster create"
}

# --- kind -------------------------------------------------------------------

# kind_keeps_config makes `create cluster` keep a copy of its --config file and
# `get nodes` report one node, records what is piped into `docker exec -i`, and
# leaves no k3d cluster of the same name in the way.
kind_keeps_config() {
  stub_when k3d "cluster list" 1
  stub_script kind <<EOF
case "\$*" in
  *"get nodes"*) echo testcluster-control-plane ;;
  *"create cluster"*)
    while [ \$# -gt 0 ]; do
      [ "\$1" = --config ] && cp "\$2" "$STUB_DIR/kind-config.yaml"
      shift
    done
    ;;
esac
EOF
  stub_script docker <<EOF
case "\$*" in
  *"exec -i"*) { printf '%s\n' "\$*"; cat; } >>"$STUB_DIR/exec.log" ;;
esac
EOF
}

@test "kind up points containerd at certs.d and writes a hosts.toml per mirror" {
  kind_keeps_config
  REGISTRY_MIRRORS="docker.io=docker-mirror.example.com,ghcr.io=http://10.0.0.5:5000" \
    run bash "$ROOT/runtimes/kind/up.sh" testcluster
  [ "$status" -eq 0 ]
  run cat "$STUB_DIR/kind-config.yaml"
  [[ "$output" == *"containerdConfigPatches:"* ]] || false
  [[ "$output" == *'config_path = "/etc/containerd/certs.d"'* ]] || false
  run cat "$STUB_DIR/exec.log"
  [[ "$output" == *"exec -i testcluster-control-plane"* ]] || false
  [[ "$output" == *"/etc/containerd/certs.d/docker.io/hosts.toml"* ]] || false
  [[ "$output" == *'server = "https://registry-1.docker.io"'* ]] || false
  [[ "$output" == *'[host."https://docker-mirror.example.com"]'* ]] || false
  [[ "$output" == *"/etc/containerd/certs.d/ghcr.io/hosts.toml"* ]] || false
  [[ "$output" == *'server = "https://ghcr.io"'* ]] || false
  [[ "$output" == *'[host."http://10.0.0.5:5000"]'* ]] || false
}

@test "kind up leaves containerd alone when REGISTRY_MIRRORS is unset" {
  kind_keeps_config
  run bash "$ROOT/runtimes/kind/up.sh" testcluster
  [ "$status" -eq 0 ]
  run cat "$STUB_DIR/kind-config.yaml"
  [[ "$output" != *"containerdConfigPatches"* ]] || false
  [ ! -e "$STUB_DIR/exec.log" ]
}

@test "kind up stops before creating a cluster when REGISTRY_MIRRORS is malformed" {
  stub_when k3d "cluster list" 1
  REGISTRY_MIRRORS="docker.io=ftp://mirror.example.com" \
    run bash "$ROOT/runtimes/kind/up.sh" testcluster
  [ "$status" -eq 1 ]
  [[ "$output" == *"REGISTRY_MIRRORS entry"* ]] || false
  refute_called kind "create cluster"
}
