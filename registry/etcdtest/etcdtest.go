// Package etcdtest holds what the etcd-backed integration tests share.
package etcdtest

// Image is the etcd server image the testcontainers integration tests start
// (registry/internal/etcd, registrar/internal/replicator). Keep it on the
// minor line of the Go client in go.mod (go.etcd.io/etcd/client/v3), and in
// step with ETCD_IMAGE in e2e/etcd-image.sh, which the kind e2e harnesses
// start (#1223).
const Image = "gcr.io/etcd-development/etcd:v3.7.2"
