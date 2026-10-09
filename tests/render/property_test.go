package render

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"slices"
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
//  2. by default the ArgoCD namespace is never a destination, however the
//     namespaces are labelled -- deploying into the control plane is a path
//     to ArgoCD admin (#6)
//  3. with the legacy opt-in it appears exactly once, as the old controller
//     rendered it
//  4. no privileged namespace is ever a destination -- the cross-tenant
//     exposure invariant
//  5. every role's first group is the generated one, so the ordering the
//     equivalence diff depends on cannot drift
//  6. no destination belongs to another tenant
//  7. every policy names its own role and a resource ArgoCD's API accepts for
//     a project role -- never `*`, which makes every ArgoCD API write to the
//     project fail validation -- and the set is the profile's: the
//     least-privilege table by default (#7), six lines with the legacy one
//  8. no role lists a group twice and no repo is listed twice, while every
//     configured group, user and repo is kept, in order. ArgoCD's API refuses
//     a project with either duplicate, so one would break every ArgoCD API
//     write to it. The generator overlaps privileged groups, tenant groups
//     and users, and repeats repos, so the dedup is exercised.
func TestPropertyRenderedAppProjectInvariants(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "defaults"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) { checkPropertyInvariants(t, legacy) })
	}
}

func checkPropertyInvariants(t *testing.T, legacy bool) {
	rng := rand.New(rand.NewSource(seed(t)))
	const argoNS = "argo-cd"
	values := map[string]any{
		"argocd":      map[string]any{"namespace": argoNS},
		"scaffolding": map[string]any{"mode": "Always"},
	}
	if legacy {
		values["argocd"].(map[string]any)["allowControlPlaneDestination"] = true
		values["roles"] = map[string]any{"profile": "extensionsCompatible"}
	}

	for i := 0; i < 60; i++ {
		t.Run(fmt.Sprintf("case-%02d", i), func(t *testing.T) {
			tenants, namespaces, privileged := randomCluster(rng, argoNS)
			// g0 and e0 are also tenant groups half the time, so a privileged
			// group the tenant lists again is a common case here.
			privGroups := []any{}
			if rng.Intn(2) == 0 {
				privGroups = []any{"g0", "e0", "platform-admins"}
			}
			byName := map[string]map[string]any{}
			for _, tn := range tenants {
				tm, _ := tn.(map[string]any)
				n, _ := tm["metadata"].(map[string]any)["name"].(string)
				byName[n] = tm
			}

			rendered, childErrs := TwoStage(t, Fixture{
				Values: values,
				Cluster: map[string]any{
					"tenants":    tenants,
					"namespaces": namespaces,
					"integrationConfig": map[string]any{
						"accessControl": map[string]any{
							"privileged": map[string]any{"namespaces": privileged, "groups": privGroups},
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
					if i >= 3 {
						// Already reported by the count check above; indexing
						// the suffix table past it would panic the binary
						// instead of failing the test.
						break
					}
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
					policies, _ := rm["policies"].([]any)
					if legacy {
						if len(policies) != len(projectScopedResources) {
							t.Errorf("%s/%v: %d policies, want %d", name, rm["name"], len(policies), len(projectScopedResources))
						}
					} else {
						want := leastPrivilegePolicies(name, suffix, false, false)
						if got := anyStrings(policies); !slices.Equal(got, want) {
							t.Errorf("%s/%v: policies %v, want %v", name, rm["name"], got, want)
						}
					}
					for _, p := range policies {
						ps, _ := p.(string)
						f := strings.Split(ps, ", ")
						if len(f) != 6 || f[1] != fmt.Sprintf("proj:%s:%v", name, rm["name"]) || !slices.Contains(projectScopedResources, f[2]) {
							t.Errorf("%s/%v: policy %q is not a valid project role policy for this role", name, rm["name"], ps)
						}
					}
					ac, _ := byName[name]["spec"].(map[string]any)["accessControl"].(map[string]any)
					block, _ := ac[map[string]string{"owner": "owners", "edit": "editors", "view": "viewers"}[suffix]].(map[string]any)
					want := uniqStrings(slices.Concat(
						[]any{groups[0]}, privGroups, anyList(block["groups"]), anyList(block["users"])))
					if got := uniqStrings(groups); len(got) != len(groups) || !slices.Equal(got, want) {
						t.Errorf("%s/%v: groups %v, want %v", name, rm["name"], groups, want)
					}
				}

				annos, _ := byName[name]["metadata"].(map[string]any)["annotations"].(map[string]any)
				var wantRepos []string
				if raw, ok := annos[sourceReposAnnotation].(string); ok {
					var list []any
					if err := json.Unmarshal([]byte(raw), &list); err != nil {
						t.Fatalf("%s: generator wrote invalid JSON %q: %v", name, raw, err)
					}
					wantRepos = uniqStrings(list)
				}
				gotRepos, _ := sp["sourceRepos"].([]any)
				if g := uniqStrings(gotRepos); len(g) != len(gotRepos) || !slices.Equal(g, wantRepos) {
					t.Errorf("%s: sourceRepos %v, want %v", name, gotRepos, wantRepos)
				}

				dests := destinationNames(sp)
				wantArgo := 0
				if legacy {
					wantArgo = 1
				}
				if n := countOf(dests, argoNS); n != wantArgo {
					t.Errorf("%s: ArgoCD namespace appears %d times, want %d: %v", name, n, wantArgo, dests)
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
		// Valid JSON, wrong leaf TYPE: without the child's kind guard these
		// stringify ("map[]", "123") and apply silently. Each must fail now.
		`[{"group":{},"kind":["x"]}]`,
		`[{"group":true,"kind":"A"}]`,
		`[{"group":"","kind":123}]`,
		`[123, true, null]`,
		`["ok", 123]`,
	}

	// Every hostile value runs against every override key. An earlier version
	// picked one key at random per value, which left two thirds of the
	// value-by-key matrix permanently unexercised at the fixed default seed.
	keys := []struct{ short, key string }{
		{"whitelist", "argocd.tenantoperator.stakater.com/cluster-resource-whitelist"},
		{"blacklist", "argocd.tenantoperator.stakater.com/namespace-resource-blacklist"},
		{"source-repos", "argocd.tenantoperator.stakater.com/source-repos"},
	}
	for i, raw := range hostile {
		for _, k := range keys {
			key := k.key
			t.Run(fmt.Sprintf("value-%02d-%s", i, k.short), func(t *testing.T) {
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
}

// Scale. The parent ranges over every Tenant and every Namespace in the
// cluster on each reconcile, and re-renders on every Namespace event, so its
// cost is the thing that grows.
//
// The shape is a large real cluster: 1000 tenants with 3 namespaces each, 7000
// namespaces that belong to no tenant, and 15 privileged patterns, one of which
// removes a namespace from every 20th tenant. With the single-pass parent the
// whole timed block -- helm template, the parent and 1000 children -- takes
// under a second; scanning the Namespace list once per tenant took over 20 s
// on the same input. The ceiling sits far from both, so it does not flake on a
// slow runner and still fails on an accidentally quadratic loop.
func TestScaleManyTenants(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped under -short")
	}
	const (
		tenants   = 1000
		perTenant = 3
		others    = 7000
		argoNS    = "argo-cd"
		ceiling   = 10 * time.Second
	)

	var ts, ns []any
	for i := 0; i < tenants; i++ {
		name := fmt.Sprintf("t%04d", i)
		ts = append(ts, map[string]any{
			"metadata": map[string]any{"name": name},
			"spec": map[string]any{"accessControl": map[string]any{
				"owners": map[string]any{"groups": []any{name + "-owners"}, "users": []any{name + "@example.com"}},
			}},
		})
		for j := 0; j < perTenant; j++ {
			ns = append(ns, map[string]any{"metadata": map[string]any{
				"name":   fmt.Sprintf("%s-ns%d", name, j),
				"labels": map[string]any{"stakater.com/tenant": name},
			}})
		}
		if i%20 == 0 {
			ns = append(ns, map[string]any{"metadata": map[string]any{
				"name":   name + "-ci",
				"labels": map[string]any{"stakater.com/tenant": name},
			}})
		}
	}
	for i := 0; i < others; i++ {
		ns = append(ns, map[string]any{"metadata": map[string]any{
			"name":   fmt.Sprintf("shared-%05d", i),
			"labels": map[string]any{"kubernetes.io/metadata.name": fmt.Sprintf("shared-%05d", i)},
		}})
	}
	privileged := []any{"^kube.*", "^openshift.*", "^default$", "-ci$"}
	for i := len(privileged); i < 15; i++ {
		privileged = append(privileged, fmt.Sprintf("^platform-%02d$", i))
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
				"accessControl": map[string]any{"privileged": map[string]any{"namespaces": privileged, "groups": []any{}}},
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
		// The -ci namespace is privileged, so every tenant ends with exactly
		// its three ordinary namespaces -- never the ArgoCD namespace (#6).
		if got := len(destinationNames(sp)); got != perTenant {
			t.Errorf("%s: %d destinations, want %d", name, got, perTenant)
		}
	}
	t.Logf("%d tenants, %d namespaces, %d privileged patterns rendered in %s",
		tenants, len(ns), len(privileged), elapsed.Round(time.Millisecond))
	if elapsed > ceiling {
		t.Errorf("render took %s, ceiling %s: the parent is scanning the Namespace list once per tenant again", elapsed.Round(time.Millisecond), ceiling)
	}
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
		// Duplicates on purpose: an owner user named like an owner group, a
		// group listed twice, and a repo listed twice.
		ownerGroups := randList(rng, "g")
		if len(ownerGroups) > 0 && rng.Intn(3) == 0 {
			ownerGroups = append(ownerGroups, ownerGroups[0])
		}
		ownerUsers := randList(rng, "u")
		if rng.Intn(3) == 0 {
			ownerUsers = append(ownerUsers, "g0")
		}
		md := map[string]any{"name": name}
		if rng.Intn(2) == 0 {
			md["annotations"] = map[string]any{
				sourceReposAnnotation: `["https://r/` + name + `","https://r/shared","https://r/` + name + `"]`,
			}
		}
		tenants = append(tenants, map[string]any{
			"metadata": md,
			"spec": map[string]any{"accessControl": map[string]any{
				"owners":  map[string]any{"groups": ownerGroups, "users": ownerUsers},
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
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	// DNS-1123 names must start AND end alphanumeric; hyphens anywhere in
	// between are legal and common, so hyphenated tenant names must stay in
	// the generated set. (An earlier version excluded '-' from every
	// position, so the common real-world shape was never fuzzed at all.)
	if b[0] == '-' {
		b[0] = 'x'
	}
	if b[n-1] == '-' {
		b[n-1] = 'x'
	}
	return string(b)
}

// anyStrings returns the string entries of a list, in order.
func anyStrings(list []any) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// anyList returns v as a list, or nil when it is absent.
func anyList(v any) []any {
	l, _ := v.([]any)
	return l
}

// uniqStrings returns the string entries of list with duplicates dropped,
// first occurrence kept -- what the child gotemplate's mustUniq does.
func uniqStrings(list []any) []string {
	var out []string
	for _, v := range list {
		s, _ := v.(string)
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
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
