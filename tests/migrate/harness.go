// Package migrate tests hack/mto-argocd-migrate against a fake cluster.
//
// The script's job is to move production AppProjects between two owners without
// deleting any of them, so the properties worth testing are the ones that hold
// it to that: what it refuses to do, what order it does things in, and what it
// does NOT run. None of those need a real API server -- they need a kubectl that
// records what it was asked to do, which is what this harness provides.
//
// Everything the shim serves is a fixture except the parent Template, which is
// rendered from the real chart. The script reads its whole configuration off
// that object -- annotation keys, scaffolding mode, ArgoCD namespace -- so a
// hand-written copy would let the chart and the script drift apart silently,
// which is the one failure this layer exists to catch.
package migrate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stakater-ab/mto-extension-argocd/tests/internal/helm"
)

const scriptPath = "../../hack/mto-argocd-migrate"

// shim is a kubectl that appends every invocation to $FAKE_LOG and answers
// `get` from files in $FAKE_DIR. Mutating verbs succeed without doing anything:
// the point is to record that they were attempted, and in what order.
//
// It does not model writes, with one exception. cutover patches
// onDeletePurgeAppProject and then reads it back, so the shim has to answer that
// read as a real cluster would after a successful patch -- and answer it the
// other way when $FAKE_DIR/retain-patch-fails exists, which is the branch that
// stops a delete from destroying an AppProject.
const shim = `#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"$FAKE_LOG"
[[ ${1:-} == --context=* ]] && shift
if [[ ${1:-} == delete && -f "$FAKE_DIR/delete-hangs" ]]; then
  echo 'error: timed out waiting for the condition on extensions/...' >&2; exit 1
fi
[[ ${1:-} == get ]] || exit 0
shift
res=""; name=""; jsonpath=""
while (( $# )); do
  case "$1" in
    -o) [[ ${2:-} == jsonpath=* ]] && jsonpath=${2#jsonpath=}; shift 2 ;;
    -n) shift 2 ;;
    -*) shift ;;
    *) if [[ -z $res ]]; then res=$1; else name=$1; fi; shift ;;
  esac
done
if [[ $jsonpath == *onDeletePurgeAppProject* ]]; then
  [[ -f "$FAKE_DIR/retain-patch-fails" ]] && printf 'true' || printf 'false'
  exit 0
fi
file=""
case "$res" in
  crd) [[ -f "$FAKE_DIR/extensions-crd-exists" ]] && exit 0 || exit 1 ;;
  templates.templates.v2.stakater.com)
    [[ -f "$FAKE_DIR/parent-template.json" ]] || exit 1
    cat "$FAKE_DIR/parent-template.json"; exit 0 ;;
  integrationconfigs.tenantoperator.stakater.com)
    [[ -f "$FAKE_DIR/ic.json" ]] || exit 1
    cat "$FAKE_DIR/ic.json"; exit 0 ;;
  templateinstances.templates.v2.stakater.com)
    [[ -f "$FAKE_DIR/child-never-ready" ]] || printf 'True'; exit 0 ;;
  extensions.tenantoperator.stakater.com) file="$FAKE_DIR/extensions.json" ;;
  tenants.tenantoperator.stakater.com) file="$FAKE_DIR/tenants.json" ;;
  appprojects.argoproj.io) file="$FAKE_DIR/appprojects.json" ;;
  *) echo '{}'; exit 0 ;;
esac
if [[ $jsonpath == *range* ]]; then jq -r '.items[].metadata.name' "$file"; exit 0; fi
if [[ -n $name ]]; then
  jq -e --arg n "$name" '.items[]|select(.metadata.name==$n)' "$file" >/dev/null 2>&1 || exit 1
  jq --arg n "$name" '.items[]|select(.metadata.name==$n)' "$file"; exit 0
fi
cat "$file"
`

// cluster is one fake cluster: a directory of fixtures plus a command log.
type cluster struct {
	dir string
	t   *testing.T
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	c := &cluster{dir: t.TempDir(), t: t}

	if err := os.MkdirAll(filepath.Join(c.dir, "bin"), 0o755); err != nil {
		t.Fatalf("creating bin dir: %v", err)
	}
	c.write("bin/kubectl", shim)
	if err := os.Chmod(filepath.Join(c.dir, "bin", "kubectl"), 0o755); err != nil {
		t.Fatalf("chmod kubectl shim: %v", err)
	}
	c.write("extensions-crd-exists", "")
	c.write("parent-template.json", renderParentTemplate(t))
	c.write("ic.json", defaultIntegrationConfig)
	c.write("extensions.json", defaultExtensions)
	c.write("tenants.json", defaultTenants)
	c.write("appprojects.json", defaultAppProjects)
	return c
}

func (c *cluster) write(name, content string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.dir, name), []byte(content), 0o644); err != nil {
		c.t.Fatalf("writing %s: %v", name, err)
	}
}

func (c *cluster) path(name string) string { return filepath.Join(c.dir, name) }

// run executes the script and returns its combined output plus every kubectl
// invocation it made, in order.
func (c *cluster) run(args ...string) (out string, calls []string, err error) {
	c.t.Helper()
	logPath := filepath.Join(c.dir, "kubectl.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		c.t.Fatalf("truncating call log: %v", err)
	}

	script, absErr := filepath.Abs(scriptPath)
	if absErr != nil {
		c.t.Fatalf("resolving script: %v", absErr)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(),
		"FAKE_DIR="+c.dir,
		"FAKE_LOG="+logPath,
		"PATH="+filepath.Join(c.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		// The fake cluster answers instantly, so the only thing the production
		// 120s readiness wait would buy this suite is 120 seconds.
		"MIGRATE_READY_TIMEOUT=4",
	)
	raw, err := cmd.CombinedOutput()

	logged, readErr := os.ReadFile(logPath)
	if readErr != nil {
		c.t.Fatalf("reading call log: %v", readErr)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return string(raw), calls, err
}

// mustRun fails the test if the script exits non-zero.
func (c *cluster) mustRun(args ...string) (string, []string) {
	c.t.Helper()
	out, calls, err := c.run(args...)
	if err != nil {
		c.t.Fatalf("%v failed: %v\n%s", args, err, out)
	}
	return out, calls
}

// renderParentTemplate renders the real chart and returns Template/mto-argocd-parent
// as JSON. The script reads the chart's configuration off this object, so the
// test has to use the real one -- a hand-written fixture would let the two drift.
func renderParentTemplate(t *testing.T, sets ...string) string {
	return renderParentTemplateWith(t, "", sets...)
}

// renderParentTemplateWith renders with a values file as well as --set flags.
// A values file is unavoidable for the seeded case: `--set group=` yields null
// and `--set-string group=` is rejected, but `group: ""` is the common shape for
// core-API kinds -- and it is what the tool's own `values` output produces.
func renderParentTemplateWith(t *testing.T, valuesYAML string, sets ...string) string {
	t.Helper()
	chart, err := helm.ChartDir()
	if err != nil {
		t.Fatalf("resolving chart dir: %v", err)
	}
	args := []string{"template", "argocd", chart,
		"--set", "argocd.namespace=" + argoNamespace,
		"--show-only", "templates/20-parent-template.yaml",
	}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	if valuesYAML != "" {
		f := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(f, []byte(valuesYAML), 0o600); err != nil {
			t.Fatalf("writing values: %v", err)
		}
		args = append(args, "-f", f)
	}
	out, err := exec.Command(helm.Bin(), args...).CombinedOutput()
	if err != nil {
		t.Fatalf("rendering parent template: %v\n%s", err, out)
	}
	j, err := yamlToJSON(out)
	if err != nil {
		t.Fatalf("converting parent template to JSON: %v", err)
	}
	return j
}

// mutating reports whether a logged kubectl invocation writes to the cluster.
func mutating(call string) bool {
	for _, verb := range []string{"apply", "annotate", "label", "patch", "delete", "create", "replace", "edit"} {
		if strings.HasPrefix(call, verb+" ") {
			return true
		}
	}
	return false
}

func indexOf(calls []string, substrings ...string) int {
	for i, c := range calls {
		matched := true
		for _, s := range substrings {
			if !strings.Contains(c, s) {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}
