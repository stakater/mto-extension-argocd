// Package admission checks that the objects this chart renders are accepted by
// a real Kubernetes API server carrying template-operator-v2's CRDs.
//
// This is the gap the render tests cannot cover. tests/render proves the
// gotemplates produce the right AppProject; it says nothing about whether the
// Template CRs carrying those gotemplates will apply. The Template CRD has
// eight CEL rules and a full structural schema -- "exactly one of gotemplate or
// helm", "each outputs entry must set exactly one combination: from-only,
// fromAll-only, or from+forEach", "keyTemplate is required when forEach is
// set", listType/listMapKey constraints on parameters, length caps -- and a
// chart that violates any of them renders green everywhere else and then fails
// at helm install against a real cluster.
//
// envtest runs a genuine kube-apiserver and etcd locally, so the CEL rules are
// evaluated by the same code that would evaluate them on cluster. No cluster,
// no network, no template-operator-v2 binary: the operator's reconciler is not
// running here, and it is not needed -- admission is what is under test.
//
// Requires the envtest binaries:
//
//	make envtest-assets      # once
//	make admission-test
//
// Skipped, loudly, when KUBEBUILDER_ASSETS is unset, so `go test ./...` on a
// fresh checkout does not fail for want of a download.
package admission

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stakater-ab/mto-extension-argocd/tests/internal/helm"
	goyaml "gopkg.in/yaml.v3"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

var cfg *rest.Config

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// println goes to stderr and is swallowed without -v, so a skip used to
		// look exactly like a pass. REQUIRE_ADMISSION_TESTS=1 turns it into a
		// hard failure; the Makefile and CI both set it, so only an ad-hoc
		// `go test ./...` on a fresh checkout is allowed to skip.
		msg := "admission tests SKIPPED: KUBEBUILDER_ASSETS is unset - run `make envtest-assets`"
		if os.Getenv("REQUIRE_ADMISSION_TESTS") != "" {
			fmt.Fprintln(os.Stderr, "FATAL: "+msg)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(0)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	cfg, err = env.Start()
	if err != nil {
		println("starting envtest:", err.Error())
		os.Exit(1)
	}
	code := m.Run()
	// A failed Stop leaks a kube-apiserver and etcd on the developer's machine;
	// say so rather than leaving them to be found in `ps` a week later. The exit
	// code stays the suite's own -- a leak is worth a line, not a red build.
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v (kube-apiserver/etcd may still be running)\n", err)
	}
	os.Exit(code)
}

func newClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

// The chart's Templates and TemplateInstance must survive the CRD's schema and
// CEL rules. Anything else the chart renders is core API and is applied too,
// so an RBAC typo is caught here as well.
func TestRenderedObjectsAreAccepted(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		sets   []string
		values string
	}{
		{name: "defaults"},
		{name: "scaffolding-always", sets: []string{"scaffolding.mode=Always"}},
		{name: "scaffolding-onlabel", sets: []string{"scaffolding.mode=OnLabel"}},
		{name: "scaffolding-never", sets: []string{"scaffolding.mode=Never"}},
		{name: "sync-off", sets: []string{"sync.mode=off"}},
		{
			// A values file, not --set. helm's --set parser reads [ and ] as
			// index syntax even inside a value, so "spec.roles[*].jwtTokens"
			// arrives mangled and the chart refuses to render. Both exemptions
			// are mandatory under strict -- see values.yaml.
			name: "sync-strict",
			values: `sync:
  mode: strict
  ignoreFields:
    - metadata.finalizers
    - spec.roles[*].jwtTokens
`,
		},
		{name: "rbac-disabled", sets: []string{"rbac.create=false"}},
		{name: "source-repos", sets: []string{"argocd.sourceRepos.pattern=https://github.com/acme/{{ tenant }}-gitops"}},
		{
			// A values file rather than --set: `--set x.group=` yields null,
			// which the schema rejects, and the empty core API group is
			// exactly what a real whitelist entry looks like.
			name: "resource-lists",
			values: `argocd:
  clusterResourceWhitelist:
    - group: ""
      kind: Namespace
  namespaceResourceBlacklist:
    - group: ""
      kind: ResourceQuota
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A namespace per case, so the namespaced objects -- which carry
			// fixed names -- cannot collide between cases.
			ns := "case-" + tc.name
			createNamespace(t, c, ctx, ns)

			sets := append([]string{
				"argocd.namespace=" + ns,
				"operatorNamespace=" + ns,
				"integrationConfig.namespace=" + ns,
			}, tc.sets...)

			for _, obj := range renderChart(t, sets, tc.values) {
				if sub := obj.GetNamespace(); sub != "" && sub != ns {
					createNamespace(t, c, ctx, sub)
				}
				if err := c.Create(ctx, obj); err != nil {
					t.Errorf("API server rejected %s/%s: %v",
						obj.GetKind(), obj.GetName(), err)
					continue
				}
				// Cluster-scoped objects are the only ones shared between
				// cases; drop them again so the next case applies its own.
				// Namespaces are deliberately left behind: without a namespace
				// controller, envtest never finishes deleting one.
				if obj.GetNamespace() == "" && obj.GetKind() != "Namespace" {
					toDelete := obj.DeepCopy()
					// Logged, not discarded: a failed delete here surfaces on
					// the NEXT case as AlreadyExists, and this line is what
					// explains it.
					t.Cleanup(func() {
						if err := c.Delete(ctx, toDelete); err != nil && !apierrors.IsNotFound(err) {
							t.Logf("cleanup: deleting %s %s: %v", toDelete.GetKind(), toDelete.GetName(), err)
						}
					})
				}
			}
		})
	}
}

func createNamespace(t *testing.T, c client.Client, ctx context.Context, name string) {
	t.Helper()
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(name)
	if err := c.Create(ctx, ns); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("creating namespace %s: %v", name, err)
	}
}

func renderChart(t *testing.T, sets []string, values string) []*unstructured.Unstructured {
	t.Helper()
	chartDir, err := helm.ChartDir()
	if err != nil {
		t.Fatalf("resolving chart dir: %v", err)
	}
	args := []string{"template", "argocd", chartDir}
	if values != "" {
		f := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(f, []byte(values), 0o600); err != nil {
			t.Fatalf("writing values: %v", err)
		}
		args = append(args, "-f", f)
	}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command(helm.Bin(), args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}

	// A real YAML stream decoder, not strings.Split on "\n---\n": a rendered
	// document that happens to carry a column-zero "---" inside a block scalar
	// -- and this chart's Templates are mostly block scalars full of gotemplate
	// text -- would be cut in half by a text split. Same approach as
	// tests/render/engine.go's Documents.
	var objs []*unstructured.Unstructured
	dec := goyaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding rendered stream: %v\n%s", err, out)
		}
		// Empty documents -- helm emits comment-only ones -- decode to nil.
		if len(doc) == 0 {
			continue
		}
		// Re-encode the split document and decode it with sigs.k8s.io/yaml,
		// which yields JSON-compatible types (int64, never int) as it always
		// has -- goyaml exists in this pipeline only for the stream
		// splitting, and unstructured's DeepCopy (used by the cleanup above)
		// panics on goyaml's plain ints.
		raw, err := goyaml.Marshal(doc)
		if err != nil {
			t.Fatalf("re-encoding rendered doc: %v", err)
		}
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(raw, obj); err != nil {
			t.Fatalf("decoding rendered doc: %v\n%s", err, raw)
		}
		// A non-empty document with no kind is a malformed render, not
		// something to skip past -- silently dropping it from admission
		// checking would shrink coverage without a trace.
		if obj.GetKind() == "" {
			t.Fatalf("rendered document has no kind:\n%s", raw)
		}
		objs = append(objs, obj)
	}
	if len(objs) == 0 {
		t.Fatal("chart rendered no objects")
	}
	return objs
}
