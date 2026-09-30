package stick_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyler-sommer/stick"
	"github.com/tyler-sommer/stick/twig"
)

// e2eLoader records Load order across the whole template group.
type e2eLoader struct {
	templates map[string]string
	calls     []string
}

func (l *e2eLoader) Load(name string) (stick.Template, error) {
	l.calls = append(l.calls, name)
	return (&stick.MemoryLoader{Templates: l.templates}).Load(name)
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// e2eWriter records non-whitespace write units, exposing output-buffer
// ordering instead of only the concatenated final string.
type e2eWriter struct{ units []string }

func (w *e2eWriter) Write(p []byte) (int, error) {
	if s := strings.TrimSpace(string(p)); s != "" {
		w.units = append(w.units, s)
	}
	return len(p), nil
}

// TestTemplateGroupEndToEnd assembles the full group demanded by the task:
//
//   - three-level extends (layout.html.twig <- page.html.twig <-
//     home.html.twig);
//   - same-name block overrides at two levels ("content") plus a
//     mid-only override ("aside");
//   - an include with shared+overlaid context and an include ... only;
//   - an imported macro rendering user data;
//   - |raw and |escape interacting with autoescape;
//   - a user function and filter resolving through the stacked Env.
//
// The test tracks, in one execution, the loader call order, the output
// buffer unit order, and the final rendered string — so a reviewer can
// trace every assertion back to a specific executor path rather than a
// single big snapshot.
func TestTemplateGroupEndToEnd(t *testing.T) {
	tpls := map[string]string{
		"layout.html.twig": `LAYOUT[` +
			`aside:{% block aside %}NONE{% endblock %}|` +
			`content:{% block content %}BASEC{% endblock %}|` +
			`foot:{{ footer() }}]`,
		"page.html.twig": `{% extends 'layout.html.twig' %}` +
			`{% block aside %}PASIDE{% endblock %}` +
			`{% block content %}PAGE({{ parent() }}){% endblock %}`,
		"home.html.twig": `{% extends 'page.html.twig' %}` +
			`{% from 'widgets.html.twig' import badge %}` +
			`{% block content %}HOME[` +
			`{{ badge(user) }}` +
			`{% include 'shared.html.twig' with {'note': '<b>note</b>'} %}` +
			`{% include 'solo.html.twig' with {'note': 'solo'} only %}` +
			`{{ parent() }}` +
			`]{% endblock %}`,
		"widgets.html.twig": `{% macro badge(name) %}<span class="badge">{{ name|escape }}</span>{% endmacro %}`,
		"shared.html.twig":  `shared{user={{ user }}|note={{ note|raw }}|extra={{ shout('go') }}}`,
		"solo.html.twig":    `solo{user={{ user|default('NOUSER') }}|note={{ note }}}`,
	}
	loader := &e2eLoader{templates: tpls}
	env := twig.New(loader)
	env.Functions["footer"] = func(stick.Context, ...stick.Value) stick.Value {
		return stick.NewSafeValue("<small>safe foot</small>", "html")
	}
	env.Functions["shout"] = func(_ stick.Context, args ...stick.Value) stick.Value {
		return strings.ToUpper(stick.CoerceString(args[0]))
	}

	w := &e2eWriter{}
	ctx := map[string]stick.Value{"user": "<a>ann</a>"}
	if err := env.Execute("home.html.twig", w, ctx); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// 1. Loader order. walkIncludeNode probes the loader for existence
	//    before execute() loads+parses the include, so each included
	//    template appears exactly twice (probe, then parse). The imported
	//    macro library loads during walkChild — BEFORE the root parent is
	//    fetched from the ModuleNode arm. The distinct-name sequence is
	//    therefore: entry, parents leaf->root up to the import point,
	//    macro library, remaining parents, then each include.
	wantCalls := []string{
		"home.html.twig", "page.html.twig", "widgets.html.twig",
		"layout.html.twig", "shared.html.twig", "solo.html.twig",
	}
	var distinct []string
	for _, c := range loader.calls {
		if !containsStr(distinct, c) {
			distinct = append(distinct, c)
		}
	}
	if strings.Join(distinct, ",") != strings.Join(wantCalls, ",") {
		t.Errorf("distinct loader order = %v\nraw calls = %v\nwant %v", distinct, loader.calls, wantCalls)
	}
	// Each include is probed then loaded: exact multiplicity check.
	countCalls := func(name string) int {
		n := 0
		for _, c := range loader.calls {
			if c == name {
				n++
			}
		}
		return n
	}
	for _, c := range []string{"shared.html.twig", "solo.html.twig"} {
		if n := countCalls(c); n != 2 {
			t.Errorf("%s loaded %d times, want 2 (existence probe + parse load)", c, n)
		}
	}

	// 2. Final output, asserted as meaningful substrings (each maps to a
	//    different code path) instead of one giant exact snapshot.
	out := strings.Join(w.units, "")
	wants := map[string]string{
		"layout shell":             "LAYOUT[aside:PASIDE|content:",
		"mid-only block wins":      "aside:PASIDE",
		"three-level parent chain": "HOME[",
		"macro escapes data once":  `<span class="badge">&lt;a&gt;ann&lt;/a&gt;</span>`,
		"shared include overlay":   `shared{user=&lt;a&gt;ann&lt;/a&gt;|note=<b>note</b>|extra=GO}`,
		"only include isolation":   `solo{user=NOUSER|note=solo}`,
		"nested parent() chain":    `PAGE(BASEC)`,
		"safe function output":     `foot:<small>safe foot</small>`,
	}
	for label, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("%s: output missing %q\nfull: %s", label, want, out)
		}
	}

	// 3. Buffer ordering: the macro/includes execute into the content block
	//    BEFORE the parent chain, and the layout static shell frames them.
	order := w.units
	index := func(s string) int {
		for i, u := range order {
			if strings.Contains(u, s) {
				return i
			}
		}
		return -1
	}
	homeI, macroI, parentI, footI := index("HOME["), index("badge"), index("PAGE(BASEC)"), index("safe foot")
	if !(homeI >= 0 && macroI > homeI && parentI > macroI && footI > parentI) {
		t.Errorf("buffer order wrong (home=%d macro=%d parent=%d foot=%d): %v", homeI, macroI, parentI, footI, order)
	}

	// 4. No double-escaping anywhere in the composed output.
	if strings.Contains(out, "&amp;") {
		t.Errorf("double-encoded ampersand in output: %s", out)
	}
}

// TestTemplateGroupDepthComparison runs the SAME leaf content once against
// a two-level chain and once against a three-level chain. The only
// permitted difference is the extra mid layer's framing; this is the
// "compare two inheritance depths" trace that catches an implementation
// special-casing depth 2 (the previous getParentBlock bug only crashed at
// depth 3).
func TestTemplateGroupDepthComparison(t *testing.T) {
	depth2 := map[string]string{
		"entry.html.twig": `{% extends 'p.html.twig' %}{% block a %}L({{ parent() }}){% endblock %}`,
		"p.html.twig":     "P{% block a %}PBLK{% endblock %}",
	}
	depth3 := map[string]string{
		"entry.html.twig": `{% extends 'm.html.twig' %}{% block a %}L({{ parent() }}){% endblock %}`,
		"m.html.twig":     `{% extends 'p.html.twig' %}{% block a %}M({{ parent() }}){% endblock %}`,
		"p.html.twig":     "P{% block a %}PBLK{% endblock %}",
	}
	render := func(tpls map[string]string) string {
		buf := &bytes.Buffer{}
		if err := twig.New(&stick.MemoryLoader{Templates: tpls}).
			Execute("entry.html.twig", buf, nil); err != nil {
			t.Fatalf("execute: %v", err)
		}
		return buf.String()
	}
	if got := render(depth2); got != "PL(PBLK)" {
		t.Errorf("depth 2 = %q, want PL(PBLK)", got)
	}
	if got := render(depth3); got != "PL(M(PBLK))" {
		t.Errorf("depth 3 = %q, want PL(M(PBLK))", got)
	}
}
