// Package render executes template-operator-v2 gotemplates the way the
// operator does, so the tests can assert on the AppProject the chart actually
// produces rather than on the Template CR that carries it.
//
// This is a deliberate mirror of the operator's renderer, not an approximation
// of it. template-operator-v2's TemplateInstance controller does exactly this:
//
//	template.New("template").Funcs(SafeSprigFuncs()).Parse(templateStr)
//
// and its SafeSprigFuncs (internal/controller/controller_utils.go, inspected
// at tov2 v0.0.6) is sprig.TxtFuncMap() with three entries deleted. Both live
// under internal/ in a private module, so they cannot be imported -- but sprig
// is public, and safeSprigFuncs below reproduces the engine exactly.
//
// The alternative, rendering gotemplates through Helm, is NOT equivalent: Helm
// overrides sprig's fromJson and toJson with its own map-only versions, adds
// toYaml/fromYaml/include/tpl/lookup/.Files, and keeps the three functions the
// operator deletes. A gotemplate can therefore pass under Helm and fail on
// cluster. Mirroring sprig directly makes that class of bug impossible: a
// Helm-only function is simply not in the map, and Parse fails with
// "function not defined".
//
// If template-operator-v2 changes SafeSprigFuncs, this drifts. It is three
// deletions; keep them in step.
package render

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"gopkg.in/yaml.v3"
)

// safeSprigFuncs mirrors template-operator-v2's SafeSprigFuncs.
//
//	env, expandenv  -- leak host environment variables to template authors
//	getHostByName   -- triggers DNS lookups (side-effect, potential SSRF)
func safeSprigFuncs() template.FuncMap {
	funcMap := sprig.TxtFuncMap()
	delete(funcMap, "env")
	delete(funcMap, "expandenv")
	delete(funcMap, "getHostByName")
	return funcMap
}

// Instance is the .instance context the operator exposes alongside .parameters.
// It is flattened into the template context by hand in Gotemplate below; no
// serialization path populates it.
type Instance struct {
	Name      string
	Namespace string
	Labels    map[string]string
}

// Gotemplate renders one gotemplate against one parameter set, exactly as
// template-operator-v2 would. An error here is an error on cluster.
func Gotemplate(gotmpl string, params map[string]any, instance Instance) (string, error) {
	t, err := template.New("template").Funcs(safeSprigFuncs()).Parse(gotmpl)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	var buf bytes.Buffer
	ctx := map[string]any{
		"parameters": params,
		"instance": map[string]any{
			"name":      instance.Name,
			"namespace": instance.Namespace,
			"labels":    instance.Labels,
		},
	}
	if err := t.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	return buf.String(), nil
}

// Documents splits a multi-document YAML stream into decoded objects, dropping
// empty documents. A gotemplate that emits nothing for a given input yields an
// empty slice rather than an error -- which is the correct outcome for, say, a
// tenant that fails the scaffolding filter.
func Documents(stream string) ([]map[string]any, error) {
	var out []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader([]byte(stream)))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode: %w", err)
		}
		if len(doc) > 0 {
			out = append(out, doc)
		}
	}
	return out, nil
}
