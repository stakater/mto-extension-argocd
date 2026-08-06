package render

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	parentTemplateName = "mto-argocd-parent"
	childTemplateName  = "mto-argocd-child"
)

// ChartDir is the chart under test, relative to this package.
func ChartDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../../charts/mto-extension-argocd")
	if err != nil {
		t.Fatalf("resolving chart dir: %v", err)
	}
	return abs
}

func helmBin() string {
	if h := os.Getenv("HELM"); h != "" {
		return h
	}
	return "helm"
}

// HelmTemplate renders the chart. It returns the error rather than failing the
// test, because several tests assert that a render is REFUSED -- the schema
// guard on argocd.namespace in particular.
func HelmTemplate(t *testing.T, valuesYAML string, sets map[string]string) (string, error) {
	t.Helper()
	args := []string{"template", "argocd", ChartDir(t)}
	if valuesYAML != "" {
		f := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(f, []byte(valuesYAML), 0o600); err != nil {
			t.Fatalf("writing values: %v", err)
		}
		args = append(args, "-f", f)
	}
	for k, v := range sets {
		args = append(args, "--set", fmt.Sprintf("%s=%s", k, v))
	}
	out, err := exec.Command(helmBin(), args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("helm template: %w: %s", err, out)
	}
	return string(out), nil
}

// MustHelmTemplate renders the chart and decodes it, failing the test on error.
func MustHelmTemplate(t *testing.T, valuesYAML string, sets map[string]string) []map[string]any {
	t.Helper()
	out, err := HelmTemplate(t, valuesYAML, sets)
	if err != nil {
		t.Fatalf("rendering chart: %v", err)
	}
	docs, err := Documents(out)
	if err != nil {
		t.Fatalf("decoding chart output: %v", err)
	}
	return docs
}

// Find returns the single object of the given kind and name.
func Find(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == kind && meta(d)["name"] == name {
			return d
		}
	}
	t.Fatalf("no %s/%s in rendered chart", kind, name)
	return nil
}

func meta(obj map[string]any) map[string]any {
	m, _ := obj["metadata"].(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func spec(obj map[string]any) map[string]any {
	s, _ := obj["spec"].(map[string]any)
	if s == nil {
		return map[string]any{}
	}
	return s
}

// marshalValues renders a values map to YAML for HelmTemplate.
func marshalValues(v map[string]any) (string, error) {
	b, err := yaml.Marshal(v)
	return string(b), err
}

// paramMap turns a spec.parameters array into the name -> value map a
// gotemplate sees. Parameters declared without a value come through as nil,
// which is what the objectFieldRef ones do until the fixture supplies them.
func paramMap(obj map[string]any) map[string]any {
	out := map[string]any{}
	list, _ := spec(obj)["parameters"].([]any)
	for _, p := range list {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _ := pm["name"].(string)
		out[name] = pm["value"]
	}
	return out
}

// Fixture is one test case: chart values, plus the cluster state the parent's
// three objectFieldRef parameters would resolve to.
type Fixture struct {
	Values  map[string]any `yaml:"values"`
	Cluster map[string]any `yaml:"cluster"`

	// ExpectChildErrors names the child TemplateInstances this fixture expects
	// to FAIL, mapped to a substring their error must contain. A child that
	// fails without being listed, or is listed and renders fine, fails the
	// test.
	//
	// This is how failure isolation is asserted: a tenant with a malformed
	// annotation must break its own child and nothing else, so the golden file
	// still contains every other tenant's AppProject.
	ExpectChildErrors map[string]string `yaml:"expectChildErrors"`
}

// FixtureNames lists every fixture on disk.
//
// Discovered rather than enumerated, deliberately. An earlier version of this
// harness kept a hand-written case list, which meant a fixture could be added
// and never run -- the coverage silently did not exist. Adding a file is now
// enough to make it a test.
func FixtureNames(t *testing.T) []string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join("..", "fixtures", "*.yaml"))
	if err != nil {
		t.Fatalf("listing fixtures: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no fixtures found in tests/fixtures")
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(filepath.Base(e), ".yaml"))
	}
	sort.Strings(names)
	return names
}

// LoadFixture reads tests/fixtures/<name>.yaml.
func LoadFixture(t *testing.T, name string) Fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "fixtures", name+".yaml"))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	var f Fixture
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
	return f
}

// AssertChildErrors checks the actual per-child failures against the fixture's
// declared expectations.
func AssertChildErrors(t *testing.T, f Fixture, actual map[string]error) {
	t.Helper()
	for name, err := range actual {
		want, expected := f.ExpectChildErrors[name]
		if !expected {
			t.Errorf("child %s failed unexpectedly: %v", name, err)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("child %s failed, but the error should mention %q: %v", name, want, err)
		}
	}
	for name := range f.ExpectChildErrors {
		if _, failed := actual[name]; !failed {
			t.Errorf("child %s was expected to fail and rendered fine", name)
		}
	}
}

// TwoStage runs both stages template-operator-v2 runs:
//
//	stage 1  parent gotemplate + cluster state  -> child TemplateInstances
//	stage 2  each child's parameters            -> one AppProject
//
// Both gotemplates are taken from the RENDERED chart rather than read out of
// files/, so a bug in how the wrapping Template CR indents or quotes them is in
// scope.
//
// Stage-2 errors are returned rather than fatal: several tests assert that ONE
// child fails while the others still render -- a malformed annotation must not
// take the fleet down.
func TwoStage(t *testing.T, f Fixture) (appProjects string, childErrs map[string]error) {
	t.Helper()

	valuesYAML, err := yaml.Marshal(f.Values)
	if err != nil {
		t.Fatalf("marshalling fixture values: %v", err)
	}
	docs := MustHelmTemplate(t, string(valuesYAML), nil)

	parent := Find(t, docs, "Template", parentTemplateName)
	child := Find(t, docs, "Template", childTemplateName)

	// -- stage 1
	parentParams := paramMap(parent)
	for k, v := range f.Cluster {
		parentParams[k] = v
	}
	parentGotmpl, _ := spec(parent)["gotemplate"].(string)
	parentNS, _ := meta(parent)["namespace"].(string)
	rendered, err := Gotemplate(parentGotmpl, parentParams, Instance{
		Name: parentTemplateName, Namespace: parentNS,
	})
	if err != nil {
		t.Fatalf("parent gotemplate: %v", err)
	}
	children, err := Documents(rendered)
	if err != nil {
		t.Fatalf("decoding child TemplateInstances: %v\n%s", err, rendered)
	}

	// -- stage 2
	childGotmpl, _ := spec(child)["gotemplate"].(string)
	childErrs = map[string]error{}
	var out []string
	for _, ti := range children {
		name, _ := meta(ti)["name"].(string)
		params := paramMap(child)
		for k, v := range paramMap(ti) {
			params[k] = v
		}
		ns, _ := meta(ti)["namespace"].(string)
		ap, err := Gotemplate(childGotmpl, params, Instance{Name: name, Namespace: ns})
		if err != nil {
			childErrs[name] = err
			continue
		}
		// Decode and re-encode. Decoding is not cosmetic -- it is the same step
		// template-operator-v2 takes before applying, so a gotemplate that
		// emits unparseable YAML fails this child here exactly as it would on
		// cluster. Re-encoding then keeps the golden files stable against
		// whitespace-only template edits, which would otherwise churn every
		// time a comment moves.
		canonical, err := canonicalYAML(ap)
		if err != nil {
			childErrs[name] = fmt.Errorf("%w\n%s", err, ap)
			continue
		}
		out = append(out, canonical)
	}
	if len(out) == 0 {
		return "", childErrs
	}
	return "---\n" + strings.Join(out, "\n---\n") + "\n", childErrs
}

// canonicalYAML decodes a rendered document and re-encodes it at a fixed
// indent. It returns an error if the render is not exactly one YAML mapping --
// a child Template is expected to emit one AppProject and nothing else.
func canonicalYAML(rendered string) (string, error) {
	docs, err := Documents(rendered)
	if err != nil {
		return "", fmt.Errorf("rendered output is not valid YAML: %w", err)
	}
	if len(docs) != 1 {
		return "", fmt.Errorf("expected exactly 1 document, got %d", len(docs))
	}
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(docs[0]); err != nil {
		return "", fmt.Errorf("re-encoding: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("closing encoder: %w", err)
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// AssertGolden compares against tests/golden/<name>.yaml.
// REGENERATE=1 rewrites it; review the diff before committing.
//
// Golden files hold the FULL AppProject, not just the fields under test.
// Applies are server-side with ForceOwnership, so deleting a line from a
// gotemplate deletes that field from every live AppProject, and a partial
// golden would not notice.
func AssertGolden(t *testing.T, name, actual string) {
	t.Helper()
	path := filepath.Join("..", "golden", name+".yaml")

	if os.Getenv("REGENERATE") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating golden dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(actual), 0o644); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
		t.Logf("regenerated %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s: %v\nrun REGENERATE=1 go test ./... and review the diff", path, err)
	}
	if string(want) != actual {
		t.Errorf("rendered AppProjects differ from %s\n--- want\n%s\n--- got\n%s", path, want, actual)
	}
}
