package meshdns

import (
	"bufio"
	"os"
	"strings"
)

// NameserversFromResolvConf reads the nameserver entries from path (e.g.
// /etc/resolv.conf). The node agent runs ClusterFirstWithHostNet, so its own
// resolv.conf points at the cluster kube-dns — the correct default upstream for the
// mesh resolver to forward non-mesh queries to when --mesh-dns-upstream is unset.
// Returns nil on any read error (the caller logs and proceeds with no upstream).
func NameserversFromResolvConf(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var ns []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "nameserver" {
			ns = append(ns, fields[1])
		}
	}
	return ns
}
