package cache

// Sixth adversarial pass on the proposal-043 stack (head 270dd5b8). Tests
// adopted from the review.
//
// The fifth pass's walk (adopted by the stack as TestPerPort_WalkOverEveryInput)
// with what it did not hold the tables to:
//
//   - a service-wide filter IMPORTED from a peer cluster (proposal 026) is a
//     filter like a local one;
//   - a route's ExtensionRef filter (path /x) is on every delivered /x;
//   - a rule for one parent port is on every request delivered on that port;
//   - the comparison with a fresh cache covers /x and /b, not only /.

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/require"
)

func s6ImportedFilters(t *testing.T, flags map[string]bool) map[string]proxy.ExtensionFilter {
	out := map[string]proxy.ExtensionFilter{}
	for k, on := range flags {
		if on {
			out[k] = r3ChainCfg(t)
		}
	}
	return out
}

func s6FreshWalk(t *testing.T, applied r5Applied, impF map[string]bool) *SnapshotCache {
	t.Helper()
	c := a043Cache(t, nil)
	c.SetImportedServiceChainFilters(s6ImportedFilters(t, impF))
	for _, k := range append(slices.Clone(r5Order), "load") {
		r5Pushes(t, applied[k])[k](c)
	}
	require.NoError(t, c.generateSnapshot(context.Background()))
	return c
}

func TestPerPort_SixthWalk(t *testing.T)         { s6Walk(t, false) }
func TestPerPort_SixthWalkUnscoped(t *testing.T) { s6Walk(t, true) }

func s6Walk(t *testing.T, unscoped bool) {
	prev := r5Unscoped
	r5Unscoped = unscoped
	t.Cleanup(func() { r5Unscoped = prev })
	seed := int64(r4Env("S6_SEED", 1))
	walks, steps := r4Env("S6_WALKS", 6), r4Env("S6_STEPS", 14)
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	found := map[string]int{}
	var history []string
	show := func(kind, detail string) {
		if found[kind] < r4Env("S6_SHOW", 1) {
			t.Logf("%s: %s\n  HISTORY (latest last):\n  %s", kind, detail, strings.Join(history[max(0, len(history)-r4Env("S6_HIST", 5)):], "\n  "))
		}
		found[kind]++
	}
	for w := 0; w < walks; w++ {
		st := r5Random(rng)
		applied := r5Applied{}
		for _, k := range append(slices.Clone(r5Order), "load") {
			applied[k] = st
		}
		impF := map[string]bool{}
		c := s6FreshWalk(t, applied, impF)
		history = []string{fmt.Sprintf("seed %d walk %d START:%s", seed, w, st.describe())}
		for step := 0; step < steps; step++ {
			var what []string
			if rng.Intn(6) == 0 {
				st, what = r5Random(rng), []string{"a new state altogether"}
			} else {
				st, what = r5Mutate(rng, st)
			}
			pushes := r5Pushes(t, st)
			newImpF := maps.Clone(impF)
			if rng.Intn(3) == 0 {
				n := r5Names[rng.Intn(len(r5Names))]
				newImpF[n[0]+"/"+n[1]] = !newImpF[n[0]+"/"+n[1]]
			}
			pushes["impfilters"] = func(c *SnapshotCache) { c.SetImportedServiceChainFilters(s6ImportedFilters(t, newImpF)) }
			order := append(slices.Clone(r5Order), "load", "impfilters")
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			order = append(order, "load")
			expire := rng.Intn(5) == 0
			history = append(history, fmt.Sprintf("step %d: %s; imported filters %v; order %v expire=%v:%s", step, strings.Join(what, "; "), newImpF, order, expire, st.describe()))
			for i, k := range order {
				if expire && i == len(order)-1 {
					c.clusterMu.Lock()
					for name, e := range c.clusters {
						if !e.absentSince.IsZero() {
							e.absentSince = time.Now().Add(-2 * c.retentionGrace())
							c.clusters[name] = e
						}
					}
					c.clusterMu.Unlock()
				}
				pushes[k](c)
				if k == "impfilters" {
					impF = newImpF
				} else {
					applied[k] = st
				}
				require.NoError(t, c.generateSnapshot(ctx))
				tables := a043Tables(t, c)
				where := fmt.Sprintf("seed %d walk %d step %d after push #%d %s", seed, w, step, i, k)
				r3NoDuplicateDomains(t, where, tables)
				for _, gap := range loopbackGaps(t, c) {
					show("LOOPBACK NOT EXHAUSTIVE", where+": "+gap+r5Dump(c))
				}
				// The fifth pass's invariants, with the imported filters
				// counted as filters.
				fst := applied["filters"].clone()
				for key, on := range impF {
					if !on {
						continue
					}
					if s, ok := fst.svcs[key]; ok {
						s.filter = true
					} else {
						parts := strings.SplitN(key, "/", 2)
						fs := &r5Svc{filter: true}
						fs.ns, fs.name = parts[0], parts[1]
						fst.svcs[key] = fs
					}
				}
				r5Invariants(t, tables, fst, applied["routes"], applied["imported"], found, func(kind, detail string) {
					show(kind, where+": "+detail+r5Dump(c))
				})
				// Route-level facts.
				for _, n := range r5Names {
					key := n[0] + "/" + n[1]
					rs, ok := applied["routes"].svcs[key]
					if !ok {
						continue
					}
					for tname, table := range tables {
						if !r5HTTPTable(tname) {
							continue
						}
						loop := tname == proxy.OutboundHTTPRouteName || tname == "cap_http_18081"
						for fi, form := range r5Forms(n[0], n[1]) {
							for _, suffix := range r5Suffixes {
								host := form + suffix
								// The ExtensionRef filter of the /x rule.
								if rs.ext && !rs.every && rs.portRul == 0 {
									if a := r5Resolve(t, table, n[0], host, "/x"); a.delivered && !a.filter {
										show(fmt.Sprintf("DELIVERED WITHOUT THE EXTENSIONREF FILTER form=%d loopback=%v", fi, loop),
											fmt.Sprintf("%s: table %s Host %s path /x -> %s (vhost %q)%s", where, tname, host, a.sig, a.vhost, r5Dump(c)))
									}
								}
								// The rule of one parent port, on that port.
								if rs.portRul != 0 && !rs.every {
									on := false
									if loop {
										on = suffix == fmt.Sprintf(":%d", rs.portRul)
									} else {
										on = tname == fmt.Sprintf("cap_http_%d", rs.portRul) && (suffix == "" || suffix == fmt.Sprintf(":%d", rs.portRul))
									}
									if on {
										a := r5Resolve(t, table, n[0], host, "/")
										if a.delivered && !slices.Contains(a.marks, fmt.Sprintf("port%d:%s", rs.portRul, key)) {
											show(fmt.Sprintf("DELIVERED WITHOUT THE PORT'S RULE form=%d loopback=%v", fi, loop),
												fmt.Sprintf("%s: table %s Host %s -> %s (vhost %q)%s", where, tname, host, a.sig, a.vhost, r5Dump(c)))
										}
									}
								}
							}
						}
					}
				}
				if k != "load" {
					continue
				}
				retained := r5Retained(c)
				fresh := s6FreshWalk(t, applied, impF)
				want := a043Tables(t, fresh)
				for tname, wt := range want {
					gt, ok := tables[tname]
					if !r5HTTPTable(tname) || !ok {
						continue
					}
					for _, n := range r5Names {
						if _, kept := retained[n[0]+"/"+n[1]]; kept {
							continue
						}
						for fi, form := range r5Forms(n[0], n[1]) {
							if fi == 0 {
								continue // the short names: the adopted walk's comparison
							}
							for _, suffix := range r5Suffixes {
								for _, path := range []string{"/x", "/b"} {
									// A /b rule names ANOTHER service: while
									// any entry is retained, the remembered
									// ports of the backend judge the backendRef
									// and a fresh node knows none (the grace,
									// not a difference of the tables).
									if path == "/b" && len(retained) > 0 {
										continue
									}
									a, b := r5Resolve(t, gt, n[0], form+suffix, path), r5Resolve(t, wt, n[0], form+suffix, path)
									if a.sig != b.sig {
										show(fmt.Sprintf("INCREMENTAL != FRESH path=%s form=%d", path, fi),
											fmt.Sprintf("%s: table %s Host %s path %s\n    incremental: %s (vhost %q)\n    fresh:       %s (vhost %q)%s\n  FRESH:%s",
												where, tname, form+suffix, path, a.sig, a.vhost, b.sig, b.vhost, r5Dump(c), r5Dump(fresh)))
									}
								}
							}
						}
					}
				}
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(found)) {
		t.Errorf("%s: %d times", k, found[k])
	}
}
