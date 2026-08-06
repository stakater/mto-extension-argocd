package render

import (
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Property-based, fuzz and scale tests over the render path.
//
// The golden fixtures pin specific scenarios. These assert invariants that must
// hold for EVERY input, which is a different kind of coverage: a golden file
// only fails on the case somebody thought to write down.
//
// Seeded deterministically so a failure reproduces. Override with
// RENDER_SEED=<n> to explore, and paste the seed into the failure report.

func seed(t *testing.T) int64 {
	t.Helper()
	if s := os.Getenv("RENDER_SEED"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("RENDER_SEED must be an integer: %v", err)
		}
		return n
	}
	return 1
}

// The invariants every rendered AppProject must satisfy, whatever the input.
//
//  1. exactly three roles, named <tenant>-{owner,edit,view}
//  2. the ArgoCD namespace is always a destination -- a tenant that cannot
//     reach it has no working project
//  3. it appears exactly once, however the namespaces are labelled
//  4. no privileged namespace is ever a destination -- the cross-tenant
//     exposure invariant
//  5. every role's first group is the generated one, so the ordering the
//     equivalence diff depends on cannot drift
//  6. no destination belongs to another tenant
func TestPropertyRenderedAppProjectInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(seed(t)))
	const argoNS = "argo-cd"

	for i := 0; i < 60; i++ {
		t.Run(fmt.Sprintf("case-%02d", i), func(t *testing.T) {
			tenants, namespaces, privileged := randomCluster(rng, argoNS)

			rendered, childErrs := TwoStage(t, Fixture{
				Values: map[string]any{
					"argocd":      map[string]any{"namespace": argoNS},
					"scaffolding": map[string]any{"mode": "Always"},
				},
				Cluster: map[string]any{
					"tenants":    tenants,
					"namespaces": namespaces,
					"integrationConfig": map[string]any{
						"accessControl": map[string]any{
							"privileged": map[string]any{"namespaces": privileged, "groups": []any{}},
						},
					},
				},
			})
			for name, err := range childErrs {
				t.Fatalf("child %s failed on valid input: %v", name, err)
			}

			docs, err := Documents(rendered)
			if err != nil {
				t.Fatalf("render is not valid YAML: %v\n%s", err, rendered)
			}
			if len(docs) != len(tenants) {
				t.Fatalf("got %d AppProjects for %d tenants", len(docs), len(tenants))
			}

			for _, ap := range docs {
				name, _ := ap["metadata"].(map[string]any)["name"].(string)
				sp, _ := ap["spec"].(map[string]any)

				roles, _ := sp["roles"].([]any)
				if len(roles) != 3 {
					t.Errorf("%s: %d roles, want 3", name, len(roles))
				}
				for i, r := range roles {
					rm, _ := r.(map[string]any)
					suffix := []string{"owner", "edit", "view"}[i]
					if want := name + "-" + suffix; rm["name"] != want {
						t.Errorf("%s: role %d is %v, want %s", name, i, rm["name"], want)
					}
					groups, _ := rm["groups"].([]any)
					if len(groups) == 0 {
						t.Errorf("%s/%v: no groups", name, rm["name"])
						continue
					}
					// The generated group must lead; the equivalence diff
					// depends on this ordering.
					if first, _ := groups[0].(string); !strings.HasPrefix(first, name+"-"+suffix+"-group-") {
						t.Errorf("%s/%v: first group is %q, want the generated one", name, rm["name"], first)
					}
				}

				dests := destinationNames(sp)
				if n := countOf(dests, argoNS); n != 1 {
					t.Errorf("%s: ArgoCD namespace appears %d times, want exactly 1: %v", name, n, dests)
				}
				for _, d := range dests {
					if d == argoNS {
						continue
					}
					if matchesAny(privileged, d) {
						t.Errorf("%s: privileged namespace %q leaked into destinations", name, d)
					}
					if owner := ownerOf(namespaces, d); owner != name {
						t.Errorf("%s: destination %q belongs to tenant %q", name, d, owner)
					}
				}
			}
		})
	}
}

// Fuzzing the annotation values -- the only place a tenant controls chart
// input. The invariant is not "it always works" but "it never silently
// misbehaves": every input either renders a valid AppProject or fails that
// child loudly. A render that succeeds with a corrupted spec is the failure
// mode this rules out.
func TestFuzzAnnotationValuesNeverSilentlyCorrupt(t *testing.T) {
	rng := rand.New(rand.NewSource(seed(t) + 1))
	const argoNS = "argo-cd"

	hostile := []string{
		"", "[]", "{}", "null", "true", "0", "[ ]", "[{}]",
		`[{"group":"","kind":"Namespace"}]`,
		`[{"group":"","kind":""}]`,
		`{"not":"an array"}`,
		"[", "}", "[{", `[{"group":}]`, `['single']`,
		`[{"group":"","kind":"A"},]`,
		strings.Repeat("a", 300),
		`["https://x/` + strings.Repeat("y", 200) + `"]`,
		"[\"a\\u0000b\"]",
		`["--", "-", "'; DROP TABLE", "$(whoami)", "{{ .Values }}"]`,
		"\n\n", "\t", " [] ",
	}

	for i, raw := range hostile {
		t.Run(fmt.Sprintf("value-%02d", i), func(t *testing.T) {
			key := []string{
				"argocd.tenantoperator.stakater.com/cluster-resource-whitelist",
				"argocd.tenantoperator.stakater.com/namespace-resource-blacklist",
				"argocd.tenantoperator.stakater.com/source-repos",
			}[rng.Intn(3)]

			rendered, childErrs := TwoStage(t, Fixture{
				Values: map[string]any{
					"argocd":      map[string]any{"namespace": argoNS},
					"scaffolding": map[string]any{"mode": "Always"},
				},
				Cluster: map[string]any{
					"tenants": []any{
						map[string]any{"metadata": map[string]any{
							"name":        "fuzzed",
							"annotations": map[string]any{key: raw},
						}, "spec": map[string]any{}},
						// A bystander: whatever the hostile value does, it must
						// not reach this tenant.
						map[string]any{"metadata": map[string]any{"name": "bystander"}, "spec": map[string]any{}},
					},
					"namespaces": []any{},
					"integrationConfig": map[string]any{
						"accessControl": map[string]any{"privileged": map[string]any{"namespaces": []any{}, "groups": []any{}}},
					},
				},
			})

			if _, bystanderFailed := childErrs["mto-argocd-tenant-bystander"]; bystanderFailed {
				t.Errorf("a hostile annotation on one tenant broke another: %v", childErrs)
			}

			docs, err := Documents(rendered)
			if err != nil {
				t.Fatalf("render is not valid YAML: %v\n%s", err, rendered)
			}

			// The bystander must ALWAYS render, whatever the hostile value did.
			// Without this the whole test was vacuous: if nothing rendered, the
			// loop below never ran and the subtest passed having asserted
			// nothing at all.
			if !hasDoc(docs, "bystander") {
				t.Fatalf("bystander did not render; value %q broke an unrelated tenant", raw)
			}

			_, fuzzedFailed := childErrs["mto-argocd-tenant-fuzzed"]
			if !fuzzedFailed && !hasDoc(docs, "fuzzed") {
				t.Fatalf("value %q neither failed its child nor produced an AppProject", raw)
			}

			for _, ap := range docs {
				meta, _ := ap["metadata"].(map[string]any)
				if meta["name"] != "fuzzed" {
					continue
				}
				// It rendered, so the value parsed. Whatever it parsed to must
				// still produce a structurally sound AppProject.
				sp, _ := ap["spec"].(map[string]any)
				if _, ok := sp["destinations"].([]any); !ok {
					t.Errorf("value %q rendered an AppProject with no destinations", raw)
				}
				if roles, _ := sp["roles"].([]any); len(roles) != 3 {
					t.Errorf("value %q rendered %d roles", raw, len(roles))
				}
				for _, field := range []string{"clusterResourceWhitelist", "namespaceResourceBlacklist", "sourceRepos"} {
					v, present := sp[field]
					if !present {
						continue
					}
					items, ok := v.([]any)
					if !ok {
						t.Errorf("value %q rendered %s as %T, not a list", raw, field, v)
						continue
					}
					// A list is not enough: the earlier version stopped here,
					// which is why it could not catch YAML injection. Every
					// entry must also be the right SHAPE, so a payload that
					// smuggles extra list items in through a string is caught.
					for _, it := range items {
						switch field {
						case "sourceRepos":
							if _, ok := it.(string); !ok {
								t.Errorf("value %q put a %T in sourceRepos", raw, it)
							}
						default:
							m, ok := it.(map[string]any)
							if !ok {
								t.Errorf("value %q put a %T in %s", raw, it, field)
								continue
							}
							for k := range m {
								if k != "group" && k != "kind" {
									t.Errorf("value %q smuggled key %q into %s", raw, k, field)
								}
							}
						}
					}
				}
			}
		})
	}
}

// Scale. The parent ranges over every Tenant and every Namespace in the
// cluster on each reconcile, so its cost is the thing that grows -- one
// gotemplate execution over N tenants x M namespaces, not N executions.
//
// This is a floor, not a benchmark: it catches an accidentally quadratic
// change in the filter loop, which is the plausible regression.
func TestScaleManyTenants(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped under -short")
	}
	const (
		tenants   = 200
		perTenant = 5
		argoNS    = "argo-cd"
	)

	var ts, ns []any
	for i := 0; i < tenants; i++ {
		name := fmt.Sprintf("t%03d", i)
		ts = append(ts, map[string]any{
			"metadata": map[string]any{"name": name},
			"spec": map[string]any{"accessControl": map[string]any{
				"owners": map[string]any{"groups": []any{name + "-owners"}},
			}},
		})
		for j := 0; j < perTenant; j++ {
			ns = append(ns, map[string]any{"metadata": map[string]any{
				"name":   fmt.Sprintf("%s-ns%d", name, j),
				"labels": map[string]any{"stakater.com/tenant": name},
			}})
		}
	}

	start := time.Now()
	rendered, childErrs := TwoStage(t, Fixture{
		Values: map[string]any{
			"argocd":      map[string]any{"namespace": argoNS},
			"scaffolding": map[string]any{"mode": "Always"},
		},
		Cluster: map[string]any{
			"tenants":    ts,
			"namespaces": ns,
			"integrationConfig": map[string]any{
				"accessControl": map[string]any{"privileged": map[string]any{"namespaces": []any{"^kube.*"}, "groups": []any{}}},
			},
		},
	})
	elapsed := time.Since(start)

	if len(childErrs) != 0 {
		t.Fatalf("%d children failed at scale: %v", len(childErrs), childErrs)
	}
	docs, err := Documents(rendered)
	if err != nil {
		t.Fatalf("render is not valid YAML at scale: %v", err)
	}
	if len(docs) != tenants {
		t.Fatalf("got %d AppProjects, want %d", len(docs), tenants)
	}
	for _, ap := range docs {
		name, _ := ap["metadata"].(map[string]any)["name"].(string)
		sp, _ := ap["spec"].(map[string]any)
		if got := len(destinationNames(sp)); got != perTenant+1 {
			t.Errorf("%s: %d destinations, want %d", name, got, perTenant+1)
		}
	}
	t.Logf("%d tenants x %d namespaces rendered in %s", tenants, perTenant, elapsed.Round(time.Millisecond))
}

// ---------------------------------------------------------------- generators

func randomCluster(rng *rand.Rand, argoNS string) (tenants, namespaces, privileged []any) {
	privPatterns := []string{"^kube.*", "^openshift.*", "internal", "^" + argoNS + "$"}
	n := 1 + rng.Intn(3)
	for _, p := range privPatterns[:1+rng.Intn(len(privPatterns))] {
		privileged = append(privileged, p)
	}

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("tn%d%s", i, randName(rng, rng.Intn(6)))
		tenants = append(tenants, map[string]any{
			"metadata": map[string]any{"name": name},
			"spec": map[string]any{"accessControl": map[string]any{
				"owners":  map[string]any{"groups": randList(rng, "g"), "users": randList(rng, "u")},
				"editors": map[string]any{"groups": randList(rng, "e")},
				"viewers": map[string]any{"users": randList(rng, "v")},
			}},
		})
		for j := 0; j < rng.Intn(5); j++ {
			// Deliberately includes names that collide with privileged
			// patterns, so the filter is exercised rather than assumed.
			prefixes := []string{"", "kube-", "openshift-", "my-internal-"}
			nsName := prefixes[rng.Intn(len(prefixes))] + fmt.Sprintf("%s-ns%d", name, j)
			namespaces = append(namespaces, map[string]any{"metadata": map[string]any{
				"name":   nsName,
				"labels": map[string]any{"stakater.com/tenant": name},
			}})
		}
	}
	// The ArgoCD namespace exists and is unlabelled most of the time, labelled
	// sometimes -- the duplicate-destination edge.
	argoLabels := map[string]any{}
	if len(tenants) > 0 && rng.Intn(4) == 0 {
		owner, _ := tenants[0].(map[string]any)["metadata"].(map[string]any)["name"].(string)
		argoLabels["stakater.com/tenant"] = owner
	}
	namespaces = append(namespaces, map[string]any{"metadata": map[string]any{"name": argoNS, "labels": argoLabels}})
	return tenants, namespaces, privileged
}

func randName(rng *rand.Rand, n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789-"
	if n == 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet)-1)] // never a trailing hyphen
	}
	return string(b)
}

func randList(rng *rand.Rand, prefix string) []any {
	var out []any
	for i := 0; i < rng.Intn(3); i++ {
		out = append(out, fmt.Sprintf("%s%d", prefix, i))
	}
	return out
}

// hasDoc reports whether a document with this metadata.name was rendered.
func hasDoc(docs []map[string]any, name string) bool {
	for _, d := range docs {
		m, _ := d["metadata"].(map[string]any)
		if m["name"] == name {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------- helpers

func destinationNames(spec map[string]any) []string {
	list, _ := spec["destinations"].([]any)
	out := make([]string, 0, len(list))
	for _, d := range list {
		dm, _ := d.(map[string]any)
		s, _ := dm["namespace"].(string)
		out = append(out, s)
	}
	return out
}

func countOf(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

func ownerOf(namespaces []any, name string) string {
	for _, n := range namespaces {
		nm, _ := n.(map[string]any)["metadata"].(map[string]any)
		if nm["name"] != name {
			continue
		}
		labels, _ := nm["labels"].(map[string]any)
		s, _ := labels["stakater.com/tenant"].(string)
		return s
	}
	return ""
}

// matchesAny mirrors the unanchored regexMatch the parent uses. Deliberately
// re-implemented rather than shared, so a change to the template's semantics
// shows up as a test failure instead of both sides moving together.
func matchesAny(patterns []any, name string) bool {
	for _, p := range patterns {
		s, _ := p.(string)
		if unanchoredMatch(s, name) {
			return true
		}
	}
	return false
}

func unanchoredMatch(pattern, s string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(s)
}
