package stick

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// scopeProbe executes tpl with a MemoryLoader and returns the rendered
// output plus a reference to the caller's initial context map, so tests
// can assert whether include/macro execution mutated the outer scope.
func scopeProbe(t *testing.T, tpls map[string]string, entry string, root map[string]Value) (string, map[string]Value, error) {
	t.Helper()
	env := New(&MemoryLoader{Templates: tpls})
	buf := &bytes.Buffer{}
	err := env.Execute(entry, buf, root)
	return buf.String(), root, err
}

// TestIncludeContextCopyNotShare pins the include context contract:
//
//   - without `only`, the included template starts from a SNAPSHOT copy of
//     the caller scope's flattened values (scopeStack.All), overlaid with
//     the `with` hash; assignments inside the include never reach the
//     caller;
//   - with `only`, the caller snapshot is replaced by an empty map and
//     only the `with` hash is visible;
//   - the snapshot is shallow: the SAME backing value for a hash variable
//     is shared, matching Twig's reference semantics for objects.
func TestIncludeContextCopyNotShare(t *testing.T) {
	tpls := map[string]string{
		"main": `{% include 'inc' with {'inner': 'IN'} %}` +
			`<after:{{ outer }}>`,
		"inc": `inc{outer={{ outer }}|inner={{ inner }}|h.k={{ h.k }}}{% set outer = 'MUT' %}`,
	}
	root := map[string]Value{
		"outer": "OUT",
		"h":     map[string]Value{"k": "K"},
	}
	out, rootCtx, err := scopeProbe(t, tpls, "main", root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "inc{outer=OUT|inner=IN|h.k=K}") {
		t.Errorf("include did not receive copied scope + with hash: %q", out)
	}
	if !strings.HasSuffix(out, "<after:OUT>") {
		t.Errorf("assignment inside include leaked into caller scope: %q", out)
	}
	// "outer" existed in the caller scope before the include; the
	// non-sharing invariant is that its VALUE in the caller map did not
	// change when the include rebound the name in its own snapshot.
	if got := rootCtx["outer"]; got != "OUT" {
		t.Errorf("include mutated caller scope variable outer = %v, want OUT", got)
	}
	// And a name created only inside the include ("inner") must not have
	// been written back to the caller map at all.
	if _, ok := rootCtx["inner"]; ok {
		t.Errorf("include-created variable %q leaked into caller context", "inner")
	}

	// only: caller scope is invisible, with hash still present.
	tpls["mainOnly"] = `{% set outer = 'OUT' %}{% include 'incOnly' with {'inner': 'IN'} only %}`
	tpls["incOnly"] = `only{outer={{ outer|default('UNDEF') }}|inner={{ inner }}|h.k={{ h.k|default('NOH') }}}`
	env := New(&MemoryLoader{Templates: tpls})
	env.Filters["default"] = func(_ Context, val Value, args ...Value) Value {
		if CoerceString(val) == "" && len(args) > 0 {
			return args[0]
		}
		return val
	}
	buf := &bytes.Buffer{}
	if err := env.Execute("mainOnly", buf, map[string]Value{"h": map[string]Value{"k": "K"}}); err != nil {
		t.Fatal(err)
	}
	if want := "only{outer=UNDEF|inner=IN|h.k=NOH}"; buf.String() != want {
		t.Errorf("only include = %q, want %q", buf.String(), want)
	}
}

// TestIncludeStartsFreshBlockStack documents block visibility across an
// include: an included template executes via a brand-new state, so it can
// neither see nor override the caller's blocks. block() on a name that
// exists only in the caller must fail in the include, while the caller
// output is otherwise unaffected.
func TestIncludeStartsFreshBlockStack(t *testing.T) {
	tpls := map[string]string{
		"main": `{% block b %}MAINB{% endblock %}|{% include 'inc' %}`,
		"inc":  `inc`,
	}
	env := New(&MemoryLoader{Templates: tpls})
	s := newState("main", io.Discard, map[string]Value{}, env)
	mainTree, err := env.load("main")
	if err != nil {
		t.Fatal(err)
	}
	s.blocks = append(s.blocks, mainTree.Blocks())
	incTree, err := env.load("inc")
	if err != nil {
		t.Fatal(err)
	}
	si := newState("inc", io.Discard, map[string]Value{}, env)
	si.blocks = append(si.blocks, incTree.Blocks())
	if blk := si.getBlock("b"); blk != nil {
		t.Fatalf("fresh include state can see caller block b: %v", blk)
	}
	if blk := s.getBlock("b"); blk == nil {
		t.Fatal("caller state lost its own block after fresh-state construction")
	}
}

// TestMacroScopeIsolation executes a macro twice: argument sets and {% set
// %} inside the body bind to the macro's pushed scope and must not survive
// the call nor leak between invocations.
func TestMacroScopeIsolation(t *testing.T) {
	tpls := map[string]string{
		"m": `{% import 'm' as selfmacros %}{% macro f(a) %}[{{ a }}{% set a = 'Z' %}{{ a }}{% set leaked = 'L' %}]{% endmacro %}` +
			`{{ selfmacros.f('X') }}{{ selfmacros.f('Y') }}{{ leaked|default('NOLEAK') }}`,
	}
	env := New(&MemoryLoader{Templates: tpls})
	env.Filters["default"] = func(_ Context, val Value, args ...Value) Value {
		if CoerceString(val) == "" {
			if len(args) > 0 {
				return args[0]
			}
		}
		return val
	}
	buf := &bytes.Buffer{}
	if err := env.Execute("m", buf, nil); err != nil {
		t.Fatal(err)
	}
	if want := "[XZ][YZ]NOLEAK"; buf.String() != want {
		t.Errorf("macro scope result = %q, want %q", buf.String(), want)
	}
}

// TestChildSetVisibleInParentBlock covers the walkChild specialization: a
// top-level {% set %} in an extending template executes against the shared
// scope BEFORE the parent root walks, so the parent's block body sees the
// value. This is the deliberate deviation from walkChild's default "do
// nothing" arm and must not be lost when child execution is refactored.
func TestChildSetVisibleInParentBlock(t *testing.T) {
	tpls := map[string]string{
		"base": `{{ greeting }}`,
		"leaf": `{% extends 'base' %}{% set greeting = 'HI' %}`,
	}
	out, _, err := scopeProbe(t, tpls, "leaf", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "HI" {
		t.Errorf("child-level set not visible in parent: got %q, want HI", out)
	}
}

// TestStackedEnvSeenByFunctionsAndFilters verifies the Env layer is the
// same instance down the include/macro path: user functions and filters
// registered on the root Env remain resolvable inside included templates
// and macros, and Context.Env() is the shared object (not a re-derived
// env with only builtins).
func TestStackedEnvSeenByFunctionsAndFilters(t *testing.T) {
	tpls := map[string]string{
		"main": `{{ shout('hi') }}|{% include 'inc' %}`,
		"inc":  `{{ 'x'|decorate }}`,
	}
	env := New(&MemoryLoader{Templates: tpls})
	env.Functions["shout"] = func(_ Context, args ...Value) Value {
		return strings.ToUpper(CoerceString(args[0])) + "!"
	}
	env.Filters["decorate"] = func(_ Context, val Value, _ ...Value) Value {
		return "[" + CoerceString(val) + "]"
	}
	buf := &bytes.Buffer{}
	if err := env.Execute("main", buf, nil); err != nil {
		t.Fatal(err)
	}
	if want := "HI!|[x]"; buf.String() != want {
		t.Errorf("stacked function/filter resolution = %q, want %q", buf.String(), want)
	}
}

// TestContextNameFollowsOrigin checks the executor's s.name switching:
// while walking a block or macro defined in a parent template (or rendering
// parent()), Context.Name() inside a user filter reports the block's
// Origin, and is restored when the walk returns.
func TestContextNameFollowsOrigin(t *testing.T) {
	tpls := map[string]string{
		"base.twig": `{{ 'v'|whereami }}`,
		"leaf.twig": `{% extends 'base.twig' %}`,
	}
	env := New(&MemoryLoader{Templates: tpls})
	var names []string
	env.Filters["whereami"] = func(ctx Context, val Value, _ ...Value) Value {
		names = append(names, ctx.Name())
		return val
	}
	buf := &bytes.Buffer{}
	if err := env.Execute("leaf.twig", buf, nil); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "base.twig" {
		t.Fatalf("Context.Name during parent walk = %v, want [base.twig]", names)
	}
}
