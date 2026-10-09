# shellcheck shell=bash
# Sourced, not run: the ONE etcd server image every etcd-backed e2e harness starts.
#
# Keep it on the minor line of the Go client in go.mod (go.etcd.io/etcd/client/v3),
# and in step with registry/etcdtest.Image, which the testcontainers integration
# tests use (#1223). Override per run with ETCD_IMAGE=<image>.
ETCD_IMAGE="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.7.2@sha256:e9afa62b1e914f02db62e5c46a7a140d8cdf026f2ed888cab4e5a142ec4d06fd}"
