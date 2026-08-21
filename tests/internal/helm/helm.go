// Package helm resolves the two facts every suite needs before it can render
// this repo's chart: which helm binary to run, and where the chart is.
//
// It holds no chart content. The chart lives in charts/mto-extension-argocd and
// nowhere else; this package only points at it.
//
// It exists because every suite resolved both facts independently and one
// of them drifted. tests/render and tests/admission read $HELM
// with a "helm" fallback; tests/e2e called bare "helm", so it rendered and
// installed the chart with whatever version happened to be on PATH while every
// other suite and CI used the pinned v3.16.3. The chart is version-sensitive on
// purpose -- templates/_helpers.tpl repeats the values.schema.json enums
// because --skip-schema-validation exists from helm 3.16 -- so that divergence
// was untested exactly where it was most expensive: the suite that installs
// into a live cluster.
package helm

import (
	"os"
	"path/filepath"
)

// chartRel locates the chart from a test package's directory. Go runs a test
// binary with the working directory set to its own package, and all four suites
// sit two levels under the repo root (tests/<suite>), so one constant serves
// them all. A suite at another depth fails loudly with "chart not found" rather
// than silently rendering something else.
const chartRel = "../../charts/mto-extension-argocd"

// Bin returns the helm binary to run: $HELM when set, otherwise whatever "helm"
// resolves to on PATH. Every Makefile target that runs a suite sets $HELM to
// the pinned bin/helm.
func Bin() string {
	if h := os.Getenv("HELM"); h != "" {
		return h
	}
	return "helm"
}

// ChartDir returns the absolute path to the chart under test.
func ChartDir() (string, error) {
	return filepath.Abs(chartRel)
}
