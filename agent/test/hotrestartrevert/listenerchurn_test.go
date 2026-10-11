package hotrestartrevert_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ldsListenerTmpl is one listener of the node's LDS file: number, port, answer.
const ldsListenerTmpl = `- "@type": type.googleapis.com/envoy.config.listener.v3.Listener
  name: pod%[1]d
  address: { socket_address: { address: 127.0.0.1, port_value: %[2]d } }
  filter_chains:
  - filters:
    - name: envoy.filters.network.http_connection_manager
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
        stat_prefix: pod%[1]d
        route_config:
          virtual_hosts:
          - name: v
            domains: ["*"]
            routes:
            - match: { prefix: "/" }
              direct_response: { status: 200, body: { inline_string: "%[3]s" } }
        http_filters:
        - name: envoy.filters.http.router
          typed_config: { "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router }
`

// adminStat reads one counter of the node's Envoy.
func (n *node) adminStat(name string) int {
	n.t.Helper()
	c := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://" + n.admin + "/stats?filter=" + name)
	require.NoError(n.t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(n.t, err)
	for _, line := range strings.Split(string(body), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == name {
			v, err := strconv.Atoi(strings.TrimSpace(v))
			require.NoError(n.t, err)
			return v
		}
	}
	n.t.Fatalf("stat %s not in %q", name, body)
	return 0
}

// answers reports what the listener on a loopback port answers, "" for none.
func answers(port int) string {
	c := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// TestListenerChurnIsNotListenersStopped holds the readiness rule of issue
// #1679 to what the node proxy does all day. Its listeners come and go with
// the node's pods (LDS), and the rule reads an increase of
// listener_manager.listener_stopped as "this Envoy serves nothing": the pod is
// not Ready at once, and is hot-restarted in place after a grace.
//
// So a listener that LDS removes, and one it replaces, must not count: with
// the pinned Envoy that counter stays where it was, and the pod stays Ready
// with the Envoy it had. (What does move it, a drain of all listeners, is
// TestDrainedEnvoyLeftBehindIsHotRestartedInPlace.)
func TestListenerChurnIsNotListenersStopped(t *testing.T) {
	n := newNode(t)
	p1, p2 := freePort(t), freePort(t)
	ldsPath := filepath.Join(t.TempDir(), "lds.yaml")
	version := 0
	writeLDS := func(listeners ...string) {
		version++
		body := "version_info: \"" + strconv.Itoa(version) + "\"\nresources:\n" + strings.Join(listeners, "")
		require.NoError(t, os.WriteFile(ldsPath+".tmp", []byte(body), 0o600))
		require.NoError(t, os.Rename(ldsPath+".tmp", ldsPath)) // Envoy watches for the move
	}
	writeLDS(fmt.Sprintf(ldsListenerTmpl, 1, p1, "one"), fmt.Sprintf(ldsListenerTmpl, 2, p2, "two"))
	// The node's bootstrap, with the two pods' listeners served over LDS
	// beside the static one.
	b, err := os.ReadFile(n.bootstrap)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(n.bootstrap, append(b, []byte(fmt.Sprintf(`node: { id: node-a, cluster: aether }
dynamic_resources:
  lds_config:
    path_config_source: { path: %s }
`, ldsPath))...), 0o600))

	a := n.start("A", podOpts{})
	n.waitAdminEpoch(0, 20*time.Second, "A's envoy never went LIVE at epoch 0")
	require.Eventually(t, a.ready, 10*time.Second, 50*time.Millisecond, "A never Ready")
	require.Eventually(t, func() bool { return answers(p1) == "one" && answers(p2) == "two" }, 20*time.Second, 50*time.Millisecond,
		"the LDS listeners never served")
	// The listener watch takes its baseline once the pod is Ready.
	time.Sleep(3 * time.Second)
	require.Zero(t, n.adminStat("listener_manager.listener_stopped"))

	// From here on the pod must be Ready at every look.
	stop, unready := make(chan struct{}), make(chan int, 1)
	go func() {
		seen := 0
		for {
			select {
			case <-stop:
				unready <- seen
				return
			case <-time.After(50 * time.Millisecond):
				if !a.ready() {
					seen++
				}
			}
		}
	}()
	// Longer than the pods' drain time (3 s), after which Envoy closes a
	// removed listener, plus the 2 s between two looks of the listener watch.
	const settle = 6 * time.Second

	n.note("a pod of the node goes away: LDS removes its listener")
	writeLDS(fmt.Sprintf(ldsListenerTmpl, 1, p1, "one"))
	require.Eventually(t, func() bool { return n.adminStat("listener_manager.listener_removed") == 1 }, 20*time.Second, 50*time.Millisecond,
		"LDS never removed the listener")
	require.Eventually(t, func() bool { return answers(p2) == "" }, 10*time.Second, 50*time.Millisecond,
		"the removed listener still serves")
	time.Sleep(settle)
	afterRemoval := n.adminStat("listener_manager.listener_stopped")

	n.note("a listener is replaced in place: its filter chain changes")
	writeLDS(fmt.Sprintf(ldsListenerTmpl, 1, p1, "one again"))
	require.Eventually(t, func() bool { return answers(p1) == "one again" }, 20*time.Second, 50*time.Millisecond,
		"LDS never replaced the listener")
	time.Sleep(settle)
	afterReplacement := n.adminStat("listener_manager.listener_stopped")
	close(stop)

	n.note("listener_stopped: %d after the removal, %d after the replacement; A ready=%v; admin LIVE epoch %d",
		afterRemoval, afterReplacement, a.ready(), n.adminEpoch())
	require.Zero(t, afterRemoval, "an LDS removal moved listener_stopped: every pod deleted on a node would make its "+
		"proxy not Ready and hot-restart it")
	require.Zero(t, afterReplacement, "an LDS replacement moved listener_stopped")
	require.Zero(t, <-unready, "the pod was not Ready while its listeners changed and the others served")
	require.False(t, n.logged("has stopped its listeners"), "the supervisor read listener churn as an envoy that serves nothing")
	require.False(t, n.logged("trigger=listeners_stopped"))
	require.Equal(t, 0, n.adminEpoch(), "the envoy was hot-restarted")
	require.Equal(t, 1, a.liveEnvoys())
	require.True(t, n.listenerAccepts())
	require.Equal(t, "one again", answers(p1))
}
