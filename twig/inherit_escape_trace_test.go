package twig_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyler-sommer/stick"
	"github.com/tyler-sommer/stick/parse"
	"github.com/tyler-sommer/stick/twig"
)

func executeTwig(t *testing.T, tpls map[string]string, entry string, ctx map[string]stick.Value) string {
	t.Helper()
	env := twig.New(&stick.MemoryLoader{Templates: tpls})
	buf := &bytes.Buffer{}
	if err := env.Execute(entry, buf, ctx); err != nil {
		t.Fatalf("execute %s: %v", entry, err)
	}
	return buf.String()
}

// TestSafeValuePreventsDoubleEscapingInheritance is the autoescape
// invariant across template composition. Data printed in a template is
// escaped once. Template-authored markup returned as a VALUE — macro
// results, parent(), block() — is already escaped at its own PrintNodes
// and must come back wrapped as safe, so the surrounding print does not
// encode the ampersands of the first escaping a second time.
func TestSafeValuePreventsDoubleEscapingInheritance(t *testing.T) {
	const danger = "<i>"

	t.Run("macro literal markup and data arg", func(t *testing.T) {
		out := executeTwig(t, map[string]string{
			"m.html.twig": `{% macro box(x) %}<b>{{ x }}</b>{% endmacro %}` +
				`{% import 'm.html.twig' as ui %}{{ ui.box(d) }}`,
		}, "m.html.twig", map[string]stick.Value{"d": danger})
		// Literal <b>/</b> authored in the template stay intact; the data
		// argument is escaped exactly once.
		if want := "<b>&lt;i&gt;</b>"; out != want {
			t.Errorf("macro output = %q, want %q", out, want)
		}
	})

	t.Run("parent buffer through three levels", func(t *testing.T) {
		out := executeTwig(t, map[string]string{
			"base.html.twig": `[base{% block a %}BASE:{{ d }}{% endblock %}]`,
			"mid.html.twig":  `{% extends 'base.html.twig' %}{% block a %}[MID:{{ d }}->{{ parent() }}]{% endblock %}`,
			"leaf.html.twig": `{% extends 'mid.html.twig' %}{% block a %}[LEAF->{{ parent() }}]{% endblock %}`,
		}, "leaf.html.twig", map[string]stick.Value{"d": danger})
		if want := "[base[LEAF->[MID:&lt;i&gt;->BASE:&lt;i&gt;]]]"; out != want {
			t.Errorf("chain output = %q, want %q", out, want)
		}
		if strings.Contains(out, "&amp;") {
			t.Errorf("double escaping present (%q) — parent() markup was not trusted", out)
		}
	})

	t.Run("block function returns safe markup", func(t *testing.T) {
		out := executeTwig(t, map[string]string{
			"p.html.twig": `{{ block('a') }}`,
			"l.html.twig": `{% extends 'p.html.twig' %}{% block a %}<em>{{ d }}</em>{% endblock %}`,
		}, "l.html.twig", map[string]stick.Value{"d": danger})
		if want := "<em>&lt;i&gt;</em>"; out != want {
			t.Errorf("block() output = %q, want %q", out, want)
		}
	})

	t.Run("explicit escape on already-safe value is idempotent", func(t *testing.T) {
		out := executeTwig(t, map[string]string{
			"s.html.twig": `{{ d|escape }}{{ d|escape|escape }}`,
		}, "s.html.twig", map[string]stick.Value{
			"d": stick.NewSafeValue("<b>ok</b>", "html"),
		})
		if want := "<b>ok</b><b>ok</b>"; out != want {
			t.Errorf("repeated escape of SafeValue = %q, want %q", out, want)
		}
	})
}

// TestSafeValueScopeSpecific proves "safe" is per content strategy: a
// value safe for html is still escaped by the js escaper, and a raw value
// from one strategy does not leak into another. This is the easy
// wrong assumption — "safe" is global.
func TestSafeValueScopeSpecific(t *testing.T) {
	env := twig.New(&stick.MemoryLoader{Templates: map[string]string{
		// .txt.twig guesses strategy "txt", for which nothing is
		// registered; the escaper returns the value untouched (documented
		// existing behavior for unknown strategies). Use explicit filter
		// arguments instead for deterministic results.
		"scope.html.twig": `{{ v|escape('html') }}|{{ v|escape('js') }}`,
	}})
	buf := &bytes.Buffer{}
	err := env.Execute("scope.html.twig", buf, map[string]stick.Value{
		"v": stick.NewSafeValue(`<x>"'`, "html"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// html pass: trusted, unchanged. js pass: html-safe is not js-safe,
	// quote characters get JS-escaped.
	if !strings.HasPrefix(buf.String(), `<x>"'|`) {
		t.Errorf("html-safe value should pass html strategy untouched, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), `\u0022`) {
		t.Errorf("html-safe value must still be js-escaped, got %q", buf.String())
	}
}

// TestAutoEscapeStrategyFollowsOrigin documents how the escape strategy
// stack is built during node visiting: the module's guessed strategy
// applies to its top-level prints, and each block node pushes its OWN
// origin's strategy. A block defined in an .html.twig parent therefore
// escapes as html even though it renders through the inheritance chain,
// while an included .js.twig template escapes as js.
func TestAutoEscapeStrategyFollowsOrigin(t *testing.T) {
	t.Run("block body uses block origin", func(t *testing.T) {
		tree, err := twig.New(&stick.MemoryLoader{Templates: map[string]string{
			"b.html.twig": `{% block a %}{{ d }}{% endblock %}`,
		}}).Parse("b.html.twig")
		if err != nil {
			t.Fatal(err)
		}
		blk := tree.Blocks()["a"]
		// The visitor must have rewritten the PrintNode inside the block
		// to escape('html') (its origin is b.html.twig).
		var found *parse.FilterExpr
		var visit func(n parse.Node)
		visit = func(n parse.Node) {
			if p, ok := n.(*parse.PrintNode); ok {
				if f, ok := p.X.(*parse.FilterExpr); ok && f.Name == "escape" {
					found = f
				}
			}
			for _, c := range n.All() {
				visit(c)
			}
		}
		visit(blk.Body)
		if found == nil {
			t.Fatal("block PrintNode was not wrapped with escape filter")
		}
		if ct := found.Args[1]; ct == nil || ct.(*parse.StringExpr).Text != "html" {
			t.Errorf("block escape strategy = %v, want html", ct)
		}
	})

	t.Run("included js template escapes as js", func(t *testing.T) {
		out := executeTwig(t, map[string]string{
			"page.html.twig":  `<script>{% include 'snippet.js.twig' %}</script>`,
			"snippet.js.twig": `var x = "{{ d }}";`,
		}, "page.html.twig", map[string]stick.Value{"d": `a"b`})
		if want := `<script>var x = "a\u0022b";</script>`; out != want {
			t.Errorf("include autoescape = %q, want %q", out, want)
		}
	})
}

// TestRawFilterOutputShape pins the |raw contract used throughout the
// templates: it returns a SafeValue trusted for every built-in strategy.
func TestRawFilterOutputShape(t *testing.T) {
	env := twig.New(nil)
	out := env.Filters["raw"](nil, `<a href="x">`)
	safe, ok := out.(stick.SafeValue)
	if !ok {
		t.Fatalf("|raw returned %T, expected SafeValue", out)
	}
	for _, ct := range []string{"html", "html_attr", "js", "css", "url"} {
		if !safe.IsSafe(ct) {
			t.Errorf("|raw result not marked safe for %s", ct)
		}
	}
	if got := stick.CoerceString(safe); got != `<a href="x">` {
		t.Errorf("|raw changed underlying value: %q", got)
	}
}
