package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

const spikeFixtures = "testdata"

// sourceReposAnnotation is the chart's default annotations.sourceRepos key.
const sourceReposAnnotation = "argocd.tenantoperator.stakater.com/source-repos"

// TestEquivalenceWithExtensionsController is cross-implementation equivalence.
//
// Every other test in this package asserts the chart against itself. This one
// asserts it against the thing it replaces. Each case under
// testdata/equivalence/ is AppProject/pe as produced by
// ExtensionsArgoCDReconciler on a live OpenShift cluster, captured alongside
// the exact Tenant, Extensions CR, IntegrationConfig (its privileged patterns
// included) and the cluster's whole Namespace list (105 namespaces, most of
// them not the tenant's) that produced it. It cannot be regenerated once the controller is
// removed -- which is why it lives in git.
//
//	plain  owner users only -- the shape most tenants have.
//	rich   groups and users on all three roles, two privileged groups in the
//	       IntegrationConfig, two sourceRepos on the Extensions CR, and an
//	       unanchored privileged pattern ("t-c") that removes one of the
//	       tenant's own namespaces. Produced by editing those objects for the
//	       capture and reverting them afterwards.
//
// The capture is reduced to what the chart reads: namespace names and the two
// labels that matter, the IntegrationConfig's accessControl and argocd
// integration. Three names were sanitized consistently in input and golden
// (the cluster ID, and a client product prefix); none of them matches a
// privileged pattern before or after.
//
// The diff must be empty except for the deltas README.md lists under
// "Differences from the Extensions controller", and all are asserted
// explicitly rather than tolerated:
//
//	metadata         no multi-tenant-operator/predicate label, which MTO puts
//	                 on objects it writes; the deletion-policy annotation that
//	                 template-operator-v2 reads when purge is off.
//
//	description      "Managed by Extensions controller"
//	              -> "Managed by mto-extension-argocd"
//
//	roles[].groups   gains the tenant's users for that role, with duplicates
//	                 dropped. Derived from the Tenant fixture, not hardcoded,
//	                 so the test stays honest if the fixture changes.
//
//	roles[].policies the controller's one line per role, whose resource is
//	                 `*`, becomes one line per project-scoped resource,
//	                 because ArgoCD's API rejects `*` there. Derived from the
//	                 controller's line, so subject, action, object and effect
//	                 are still compared.
//
// It also pins that the two resource lists are seeded into chart values from
// the IntegrationConfig's deprecated .spec.integrations.argocd, and that the
// Extensions CR's sourceRepos carried into the Tenant's source-repos
// annotation render the same list.
// The comparison runs with the compatibility values (allowControlPlaneDestination,
// roles.profile=extensionsCompatible): that is the configuration a migration
// starts from. The secure defaults differ on purpose; security_test.go pins
// them.
func TestEquivalenceWithExtensionsController(t *testing.T) {
	for _, c := range []string{"plain", "rich"} {
		t.Run(c, func(t *testing.T) { checkEquivalence(t, filepath.Join("equivalence", c)) })
	}
}

func checkEquivalence(t *testing.T, dir string) {
	tenant := readJSON(t, filepath.Join(dir, "tenant.json"))
	ic := readJSON(t, filepath.Join(dir, "integrationconfig.json"))
	ext := readJSON(t, filepath.Join(dir, "extensions.json"))
	want := readJSON(t, filepath.Join(dir, "golden-appproject.json"))

	namespaces, _ := readJSON(t, filepath.Join(dir, "namespaces.json"))["items"].([]any)
	if len(namespaces) < 100 {
		t.Fatalf("namespace fixture has %d items; it should be the captured cluster-wide list", len(namespaces))
	}
	tenantName, _ := mapAt(t, tenant, "metadata")["name"].(string)

	icSpec := mapAt(t, ic, "spec")
	argoIntegration := mapAt(t, mapAt(t, icSpec, "integrations"), "argocd")
	argoNS, _ := argoIntegration["namespace"].(string)

	// The controller reads sourceRepos from the Extensions CR; the chart reads
	// them from the Tenant's annotation. Carry them across the way a migration
	// does. Any other Extensions override has no mapping here, so refuse it
	// rather than compare against a reference the chart was never given.
	extArgo, _ := mapAt(t, ext, "spec")["argoCD"].(map[string]any)
	appProject, _ := extArgo["appProject"].(map[string]any)
	for k := range appProject {
		if k != "sourceRepos" {
			t.Fatalf("extensions.json carries appProject.%s, which this test does not map onto the chart", k)
		}
	}
	if repos, ok := appProject["sourceRepos"]; ok {
		b, err := json.Marshal(repos)
		if err != nil {
			t.Fatal(err)
		}
		md := mapAt(t, tenant, "metadata")
		annos, _ := md["annotations"].(map[string]any)
		if annos == nil {
			annos = map[string]any{}
		}
		annos[sourceReposAnnotation] = string(b)
		md["annotations"] = annos
	}

	got := renderOne(t, Fixture{
		Values: map[string]any{
			"argocd": map[string]any{
				"namespace": argoNS,
				// Seeded from the deprecated IntegrationConfig fields.
				"clusterResourceWhitelist":   argoIntegration["clusterResourceWhitelist"],
				"namespaceResourceBlacklist": argoIntegration["namespaceResourceBlacklist"],
				// The legacy controller made the ArgoCD namespace a destination
				// for every tenant (#6); the chart only does that on request.
				"allowControlPlaneDestination": true,
			},
			"scaffolding": map[string]any{"mode": "Always"},
			// And its roles were the six-line `*`/`get` shape (#7).
			"roles": map[string]any{"profile": "extensionsCompatible"},
		},
		Cluster: map[string]any{
			"tenants":           []any{tenant},
			"namespaces":        namespaces,
			"integrationConfig": icSpec,
		},
	})

	wantSpec := mapAt(t, want, "spec")
	gotSpec := mapAt(t, got, "spec")
	usersByRole := tenantUsersByRole(t, tenant)

	if filepath.Base(dir) == "rich" {
		// The rich case exists to cover what plain cannot. If a re-capture
		// lost any of it, every assertion below would still pass while
		// testing less, so demand each property explicitly.
		t.Run("rich capture covers what it claims", func(t *testing.T) {
			groupsByRole := tenantListByRole(t, tenant, "groups")
			for _, role := range []string{"owner", "edit", "view"} {
				if len(usersByRole[role]) == 0 {
					t.Errorf("no %s users", role)
				}
				if len(groupsByRole[role]) == 0 {
					t.Errorf("no %s groups", role)
				}
			}
			priv := mapAt(t, mapAt(t, icSpec, "accessControl"), "privileged")
			if len(strs(t, priv["groups"])) == 0 {
				t.Error("no privileged groups in the IntegrationConfig")
			}
			if len(strs(t, wantSpec["sourceRepos"])) == 0 {
				t.Error("no sourceRepos in the reference")
			}
			// The controller's evidence for unanchored matching: a tenant
			// namespace that a privileged pattern matches only as a substring,
			// and that the controller left out of the destinations.
			if ns, p := unanchoredOnlyMatch(t, namespaces, tenantName, strs(t, priv["namespaces"])); ns == "" {
				t.Error("no tenant namespace is matched by a privileged pattern only when the pattern is unanchored")
			} else if slices.Contains(destinationNamespaces(wantSpec), ns) {
				t.Errorf("the reference keeps %s, which privileged pattern %q matches unanchored", ns, p)
			}
		})
	}

	t.Run("metadata differs only by the predicate label and the deletion-policy annotation", func(t *testing.T) {
		for _, k := range []string{"apiVersion", "kind"} {
			if want[k] != got[k] {
				t.Errorf("%s = %v, want %v", k, got[k], want[k])
			}
		}
		wantMeta := mapAt(t, want, "metadata")
		gotMeta := mapAt(t, got, "metadata")
		// The name and namespace must be the controller's, or the chart
		// renders a second AppProject instead of taking over the existing one.
		assertSameKeys(t, "metadata", wantMeta, gotMeta, "labels", "annotations")
		for _, k := range []string{"name", "namespace"} {
			if wantMeta[k] != gotMeta[k] {
				t.Errorf("metadata.%s = %v, want %v", k, gotMeta[k], wantMeta[k])
			}
		}
		if w := normalize(wantMeta["labels"]); !reflect.DeepEqual(w, map[string]any{"multi-tenant-operator/predicate": ""}) {
			t.Fatalf("reference labels changed under us: %v", w)
		}
		if l, ok := gotMeta["labels"]; ok {
			t.Errorf("metadata.labels = %v, want none", l)
		}
		if _, ok := wantMeta["annotations"]; ok {
			t.Fatalf("reference annotations changed under us: %v", wantMeta["annotations"])
		}
		if a := normalize(gotMeta["annotations"]); !reflect.DeepEqual(a, map[string]any{"templates.v2.stakater.com/deletion-policy": "Orphan"}) {
			t.Errorf("metadata.annotations = %v, want only the deletion-policy Orphan annotation", a)
		}
	})

	t.Run("no unexpected fields", func(t *testing.T) {
		// sourceRepos is compared on its own below: null in the reference and
		// absent here is the same thing to the API server.
		assertSameKeys(t, "spec", wantSpec, gotSpec, "sourceRepos")
	})

	t.Run("sourceRepos identical", func(t *testing.T) {
		if w, g := strs(t, wantSpec["sourceRepos"]), strs(t, gotSpec["sourceRepos"]); !reflect.DeepEqual(w, g) {
			t.Errorf("sourceRepos differ\n want %v\n  got %v", w, g)
		}
	})

	t.Run("description is the one deliberate change", func(t *testing.T) {
		if wantSpec["description"] != "Managed by Extensions controller" {
			t.Fatalf("reference description changed under us: %v", wantSpec["description"])
		}
		if got, want := gotSpec["description"], "Managed by mto-extension-argocd"; got != want {
			t.Errorf("description = %v, want %v", got, want)
		}
	})

	t.Run("destinations identical", func(t *testing.T) {
		// Compared as sets of whole entries: the controller's order comes from
		// an API-server LIST and carries no meaning, but every field does.
		if w, g := destinations(t, wantSpec), destinations(t, gotSpec); !reflect.DeepEqual(w, g) {
			t.Errorf("destinations differ\n want %v\n  got %v", w, g)
		}
	})

	for _, field := range []string{"clusterResourceWhitelist", "namespaceResourceBlacklist"} {
		t.Run(field+" identical", func(t *testing.T) {
			if !reflect.DeepEqual(normalize(wantSpec[field]), normalize(gotSpec[field])) {
				t.Errorf("%s differs\n want %v\n  got %v", field, wantSpec[field], gotSpec[field])
			}
		})
	}

	t.Run("roles differ only by the tenant's users and the policy expansion", func(t *testing.T) {
		// The users delta is the point of this subtest. If the fixture's shape
		// drifted and the lookup quietly decoded to empty, expect and got would
		// BOTH have no users and the assertion would pass while testing
		// nothing -- both captures have owner users, so demand them.
		if len(usersByRole["owner"]) == 0 {
			t.Fatal("fixture drift: tenant.json should carry at least one owner user; the users delta below would be vacuously empty")
		}
		wantRoles := roleList(t, wantSpec)
		gotRoles := roleList(t, gotSpec)

		// In order, like the groups below: a reordering would show up as
		// noise in every future diff.
		if w, g := roleNames(wantRoles), roleNames(gotRoles); !slices.Equal(w, g) {
			t.Fatalf("roles\n want %v\n  got %v", w, g)
		}

		for i, wr := range wantRoles {
			gr := gotRoles[i]
			name, _ := wr["name"].(string)
			assertSameKeys(t, "role "+name, wr, gr)
			if !reflect.DeepEqual(normalize(wr["description"]), normalize(gr["description"])) {
				t.Errorf("role %s .description differs\n want %v\n  got %v", name, wr["description"], gr["description"])
			}
			// Same guard as the users delta: if the reference ever stopped
			// using `*`, the expansion below would be a no-op and prove nothing.
			wantPolicies := strs(t, wr["policies"])
			if !hasStarResource(wantPolicies) {
				t.Fatalf("reference role %s has no `*`-resource policy: %v", name, wantPolicies)
			}
			if w, g := expandStarResource(wantPolicies), strs(t, gr["policies"]); !reflect.DeepEqual(w, g) {
				t.Errorf("role %s policies\n want %v\n  got %v", name, w, g)
			}

			// The whole delta: the same groups, in the same order, with this
			// role's users appended and duplicates dropped. Order is
			// asserted, not just membership -- a reordering would show up as
			// noise in every future diff.
			role := name[len(tenantName)+1:] // "pe-owner" -> "owner"
			expect := dedup(append(append([]string{}, strs(t, wr["groups"])...), usersByRole[role]...))
			if g := strs(t, gr["groups"]); !reflect.DeepEqual(expect, g) {
				t.Errorf("role %s groups\n want %v\n  got %v", name, expect, g)
			}
		}
	})
}

// unanchoredOnlyMatch returns a namespace labelled for tenant that one of the
// privileged patterns matches as a substring but not as a whole name, and that
// pattern. It returns "" when there is none.
func unanchoredOnlyMatch(t *testing.T, namespaces []any, tenant string, patterns []string) (string, string) {
	t.Helper()
	for _, ns := range namespaces {
		md, _ := ns.(map[string]any)["metadata"].(map[string]any)
		labels, _ := md["labels"].(map[string]any)
		name, _ := md["name"].(string)
		if labels["stakater.com/tenant"] != tenant {
			continue
		}
		for _, p := range patterns {
			if regexp.MustCompile(p).MatchString(name) && !regexp.MustCompile("^(?:"+p+")$").MatchString(name) {
				return name, p
			}
		}
	}
	return "", ""
}

// dedup drops repeated entries, first occurrence kept.
func dedup(in []string) []string {
	var out []string
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// projectScopedResources are the resources ArgoCD accepts in a project role
// policy, in the order the child gotemplate emits them.
var projectScopedResources = []string{"applications", "applicationsets", "logs", "exec", "repositories", "clusters"}

// expandStarResource rewrites each "p, sub, *, act, obj, eft" policy into one
// line per project-scoped resource -- the chart's deliberate policy change. A
// policy whose resource is not `*` is kept as it is.
func expandStarResource(policies []string) []string {
	var out []string
	for _, p := range policies {
		f := strings.Split(p, ", ")
		if len(f) != 6 || f[2] != "*" {
			out = append(out, p)
			continue
		}
		for _, res := range projectScopedResources {
			out = append(out, strings.Join([]string{f[0], f[1], res, f[3], f[4], f[5]}, ", "))
		}
	}
	return out
}

func hasStarResource(policies []string) bool {
	for _, p := range policies {
		if f := strings.Split(p, ", "); len(f) == 6 && f[2] == "*" {
			return true
		}
	}
	return false
}

// renderOne runs a fixture that is expected to produce exactly one AppProject.
func renderOne(t *testing.T, f Fixture) map[string]any {
	t.Helper()
	rendered, childErrs := TwoStage(t, f)
	for name, err := range childErrs {
		t.Fatalf("child %s failed: %v", name, err)
	}
	docs, err := Documents(rendered)
	if err != nil {
		t.Fatalf("decoding render: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 AppProject, got %d", len(docs))
	}
	return docs[0]
}

// tenantUsersByRole maps owner/edit/view to the Tenant's users for that role.
func tenantUsersByRole(t *testing.T, tenant map[string]any) map[string][]string {
	t.Helper()
	return tenantListByRole(t, tenant, "users")
}

// tenantListByRole maps owner/edit/view to the Tenant's users or groups for
// that role.
func tenantListByRole(t *testing.T, tenant map[string]any, field string) map[string][]string {
	t.Helper()
	ac := mapAt(t, mapAt(t, tenant, "spec"), "accessControl")
	out := map[string][]string{}
	for role, key := range map[string]string{"owner": "owners", "edit": "editors", "view": "viewers"} {
		group, _ := ac[key].(map[string]any)
		out[role] = strs(t, group[field])
	}
	return out
}

func roleList(t *testing.T, spec map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	list, _ := spec["roles"].([]any)
	for _, r := range list {
		rm, _ := r.(map[string]any)
		out = append(out, rm)
	}
	if len(out) == 0 {
		t.Fatal("no roles")
	}
	return out
}

func roleNames(roles []map[string]any) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		name, _ := r["name"].(string)
		out = append(out, name)
	}
	return out
}

// destinations returns every destination entry, all its fields included, as
// sorted JSON.
func destinations(t *testing.T, spec map[string]any) []string {
	t.Helper()
	list, _ := spec["destinations"].([]any)
	out := make([]string, 0, len(list))
	for _, d := range list {
		b, err := json.Marshal(normalize(d))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	sort.Strings(out)
	return out
}

// destinationNamespaces returns the namespace of every destination.
func destinationNamespaces(spec map[string]any) []string {
	list, _ := spec["destinations"].([]any)
	out := make([]string, 0, len(list))
	for _, d := range list {
		dm, _ := d.(map[string]any)
		ns, _ := dm["namespace"].(string)
		out = append(out, ns)
	}
	return out
}

func assertSameKeys(t *testing.T, path string, want, got map[string]any, ignore ...string) {
	t.Helper()
	skip := map[string]bool{}
	for _, k := range ignore {
		skip[k] = true
	}
	for k, v := range want {
		if skip[k] || v == nil {
			continue
		}
		if _, ok := got[k]; !ok {
			t.Errorf("%s.%s present in the controller's output and missing here", path, k)
		}
	}
	for k := range got {
		if skip[k] {
			continue
		}
		if _, ok := want[k]; !ok {
			t.Errorf("%s.%s emitted here and absent from the controller's output", path, k)
		}
	}
}

// normalize round-trips through JSON so values decoded from YAML and from JSON
// compare equal regardless of which numeric or map type each decoder chose.
func normalize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

func strs(t *testing.T, v any) []string {
	t.Helper()
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, i := range list {
		s, _ := i.(string)
		out = append(out, s)
	}
	return out
}

func mapAt(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	out, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not a mapping", key)
	}
	return out
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(spikeFixtures, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return b
}

func readJSON(t *testing.T, name string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(readFile(t, name), &out); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return out
}
