package stick

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestChildImportVisibleInParentBlock locks the walkChild fix: macro
// bindings created by {% import %} / {% from import %} at the top level of
// an extending template must be populated before the parent root walks, so
// the parent's block can invoke them. Before the fix walkChild silently
// dropped both node kinds and the parent block failed with "Undeclared
// function".
func TestChildImportVisibleInParentBlock(t *testing.T) {
	t.Run("from import", func(t *testing.T) {
		env := New(&MemoryLoader{Templates: map[string]string{
			"base": `{{ greet('x') }}`,
			"m":    `{% macro greet(a) %}HI {{ a }}{% endmacro %}`,
			"leaf": `{% extends 'base' %}{% from 'm' import greet %}`,
		}})
		buf := &bytes.Buffer{}
		if err := env.Execute("leaf", buf, nil); err != nil {
			t.Fatalf("parent block could not see child from-import: %v", err)
		}
		if buf.String() != "HI x" {
			t.Errorf("got %q, want HI x", buf.String())
		}
	})

	t.Run("import as", func(t *testing.T) {
		env := New(&MemoryLoader{Templates: map[string]string{
			"base": `{{ ui.greet('y') }}`,
			"m":    `{% macro greet(a) %}HEY {{ a }}{% endmacro %}`,
			"leaf": `{% extends 'base' %}{% import 'm' as ui %}`,
		}})
		buf := &bytes.Buffer{}
		if err := env.Execute("leaf", buf, nil); err != nil {
			t.Fatalf("parent block could not see child import-as: %v", err)
		}
		if buf.String() != "HEY y" {
			t.Errorf("got %q, want HEY y", buf.String())
		}
	})
}

// TestParentChainNoStackOverflow is the direct regression test for the
// getParentBlock-after-rewrite bug: at depth 3 the leaf's parent() resolved
// to mid correctly, but mid's own nested parent() re-resolved to mid
// forever and blew the goroutine stack. A timeout guard makes a recursion
// regression fail as a normal test error instead of crashing the binary.
func TestParentChainNoStackOverflow(t *testing.T) {
	done := make(chan struct{})
	var out string
	var execErr error
	go func() {
		defer close(done)
		env := New(&MemoryLoader{Templates: map[string]string{
			"base": `{% block a %}BASE{% endblock %}`,
			"mid":  `{% extends 'base' %}{% block a %}MID{{ parent() }}{% endblock %}`,
			"leaf": `{% extends 'mid' %}{% block a %}LEAF{{ parent() }}{% endblock %}`,
		}})
		buf := &bytes.Buffer{}
		execErr = env.Execute("leaf", buf, nil)
		out = buf.String()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("three-level parent() chain did not return (probable infinite recursion)")
	}
	if execErr != nil {
		t.Fatal(execErr)
	}
	if want := "LEAFMIDBASE"; out != want {
		t.Errorf("chain = %q, want %q", out, want)
	}
}

// escapeCalls records how many times the user escape filter actually
// transformed its input (vs. short-circuiting on an already-safe value).
type escapeCounter struct{ transformed int }

// userEscape mirrors the twig escape filter contract in the plain stick
// package: values already safe for html pass through unchanged; raw values
// get the marker & ampersand escaping and come back as SafeValue.
func userEscape(c *escapeCounter) Filter {
	return func(_ Context, val Value, _ ...Value) Value {
		if sv, ok := val.(SafeValue); ok && sv.IsSafe("html") {
			return val
		}
		c.transformed++
		return NewSafeValue("ESC("+CoerceString(val)+")", "html")
	}
}

// TestMacroAndParentNotReEscaped proves the trusted-markup wrapping lives
// at the EXECUTOR boundary and is not a property of the twig subpackage:
// using only the core package plus a user-defined escape filter, printing
// a macro result or a parent() buffer must invoke the escaper zero extra
// times for the template-authored layer. The data printed INSIDE the macro
// still escapes exactly once.
func TestMacroAndParentNotReEscaped(t *testing.T) {
	t.Run("macro result", func(t *testing.T) {
		counter := &escapeCounter{}
		env := New(&MemoryLoader{Templates: map[string]string{
			"m": `{% macro box(x) %}<b>{{ x|escape }}</b>{% endmacro %}{% import 'm' as ui %}{{ ui.box(d)|escape }}`,
		}})
		env.Filters["escape"] = userEscape(counter)
		buf := &bytes.Buffer{}
		if err := env.Execute("m", buf, map[string]Value{"d": "<i>"}); err != nil {
			t.Fatal(err)
		}
		// Exactly one transformation: the raw data inside the macro. The
		// outer |escape sees template-authored safe markup and no-ops.
		if counter.transformed != 1 {
			t.Errorf("escape transformed %d value(s), want exactly 1 (raw data only)", counter.transformed)
		}
		if want := "<b>ESC(<i>)</b>"; buf.String() != want {
			t.Errorf("macro escaping result = %q, want %q", buf.String(), want)
		}
	})

	t.Run("parent buffer", func(t *testing.T) {
		counter := &escapeCounter{}
		env := New(&MemoryLoader{Templates: map[string]string{
			"base": `{% block a %}BASE:{{ d|escape }}{% endblock %}`,
			"leaf": `{% extends 'base' %}{% block a %}LEAF:{{ parent()|escape }}{% endblock %}`,
		}})
		env.Filters["escape"] = userEscape(counter)
		buf := &bytes.Buffer{}
		if err := env.Execute("leaf", buf, map[string]Value{"d": "<i>"}); err != nil {
			t.Fatal(err)
		}
		if counter.transformed != 1 {
			t.Errorf("escape transformed %d value(s), want exactly 1 across parent() boundary", counter.transformed)
		}
		if !strings.HasPrefix(buf.String(), "LEAF:BASE:ESC(") {
			t.Errorf("parent() buffer got re-escaped: %q", buf.String())
		}
	})
}
