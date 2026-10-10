package mesh

import "time"

// AppConnectTimeout bounds the node proxy's connect to a pod's application (the
// per-pod app_<namespace>_<pod>_<port> cluster: 127.0.0.1:<port> inside the pod netns, or a
// UDS). It is the one place the destination side of a pod teardown can hang.
//
// Teardown order under containerd (aether#1103). StopPodSandbox stops the
// containers, then tears the network down through go-cni, which DELs its
// networks in the order it holds them: the built-in loopback network FIRST
// (the loopback plugin's DEL sets lo DOWN), then the pod's conflist (aether
// first, then the primary CNI, which deletes the veth). So for a short window
// the pod still has its veth, the node proxy's inbound listener still accepts
// requests a stale source sends, and the app hop dials 127.0.0.1 through a
// DOWN loopback: the SYN is dropped, not refused. With Envoy's default 5 s
// connect timeout the 503 UF was written after the veth was gone
// (ENETUNREACH), and the source waited on its own liveness timer. Measured on
// talos-main 2026-10-01 (TRIPLE roll): app gone -> fast "connection refused"
// 503s, then lo DOWN -> 5 s connection_timeout 503s, then CNI DEL, then the
// veth ~2.2 s later.
//
// A loopback connect is answered in microseconds while lo is up (accepted or
// refused), so the only steady-state connect this bound can cut is an app
// whose listen backlog is full (the kernel drops the SYN; the first retransmit
// is at 1 s). Such a request fails 503 UF, which never reached the app, so the
// source's retry policy (retriable 503, previous_hosts) moves it to another
// endpoint.
//
// The bound must stay well inside the CNI DEL's own hold: aether's DEL waits
// up to the plugin's readyProbeDelTimeout for the pod's listener to refuse a
// connect, and with lo DOWN no probe can, so the DEL returns (and the primary
// CNI deletes the veth) no earlier than that budget after it started. The
// plugin pins readyProbeDelTimeout >= 2*AppConnectTimeout.
const AppConnectTimeout = time.Second
