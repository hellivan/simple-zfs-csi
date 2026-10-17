#!/bin/sh
# Runs the ZFS conformance suite (internal/zpool/zfsconf) against the real pool,
# through the toolbox pod (its `zfs` runs on the host). Run it after changing
# internal/zpool/memzfs or anything the suite pins, and whenever the ZFS version
# on the nodes changes. Everything happens under a dataset containing
# "verify_zfs_functions"; the suite refuses to run elsewhere.
#
#   KUBE_CONTEXT=admin@kube-sl-home ZFS_CONF_ROOT=spinning-archive/verify_zfs_functions hack/run-zfs-conformance.sh
set -eu
ctx="${KUBE_CONTEXT:-admin@kube-sl-home}"
ns="${NAMESPACE:-simple-zfs-csi}"
root="${ZFS_CONF_ROOT:-spinning-archive/verify_zfs_functions}"
bin="$(mktemp)"
trap 'rm -f "$bin"' EXIT
cd "$(dirname "$0")/.."
CGO_ENABLED=0 go test -c -tags zfsreal -o "$bin" ./internal/zpool/zfsconf
pod="$(kubectl --context "$ctx" -n "$ns" get pod -l app.kubernetes.io/component=toolbox -o jsonpath='{.items[0].metadata.name}')"
kubectl --context "$ctx" -n "$ns" cp "$bin" "$pod:/tmp/zfsconf.test"
kubectl --context "$ctx" -n "$ns" exec "$pod" -- sh -c \
  "chmod +x /tmp/zfsconf.test && ZFS_CONF_ROOT='$root' /tmp/zfsconf.test -test.v; rc=\$?; rm -f /tmp/zfsconf.test; exit \$rc"
