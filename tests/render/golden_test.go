package render

import "testing"

// Every fixture in tests/fixtures/ renders to its golden file in tests/golden/.
// Cases are discovered, not listed -- adding a fixture is enough to make it a
// test. Each fixture's header comment says what it pins.
//
// Note there is no "does this gotemplate use a Helm-only function" check
// anywhere, and none is needed: the engine in engine.go is
// template-operator-v2's own function map, so toYaml, include, tpl, .Files and
// the rest are simply not defined and Parse fails. That guard is structural
// rather than a denylist somebody has to remember to update.
func TestGolden(t *testing.T) {
	for _, name := range FixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			f := LoadFixture(t, name)
			actual, childErrs := TwoStage(t, f)
			AssertChildErrors(t, f, childErrs)
			AssertGolden(t, name, actual)
		})
	}
}
