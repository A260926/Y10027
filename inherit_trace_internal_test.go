package stick

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/tyler-sommer/stick/parse"
)

// recordingLoader records every Load call in order and delegates to a
// MemoryLoader. It is the single observability point for "in which order
// does the executor ask the loader for templates" across the
// extends/include paths.
type recordingLoader struct {
	inner *MemoryLoader
	calls []string
}

func (l *recordingLoader) Load(name string) (Template, error) {
	l.calls = append(l.calls, name)
	return l.inner.Load(name)
}

// writeRecordingWriter records Write call boundaries as events. Block,
// macro, set-body and parent() rendering all swap state.out for a
// bytes.Buffer, so writes must interleave with surrounding output in a
// precise order; observing the raw Write stream (rather than the final
// string) proves buffering really happens instead of a lucky concat.
type writeRecordingWriter struct {
	events []string
}

func (w *writeRecordingWriter) Write(p []byte) (int, error) {
	w.events = append(w.events, string(p))
	return len(p), nil
}

const (
	traceBase = `[base:{% block a %}BASEA{% endblock %}|{% block b %}BASEB{% endblock %}]`
	traceMid  = `{% extends 'base.twig' %}{% block a %}MIDA({{ parent() }}){% endblock %}`
	traceLeaf = `{% extends 'mid.twig' %}{% block a %}LEAFA({{ parent() }}){% endblock %}{% block b %}LEAFB{% endblock %}`
)

func traceLoader() *recordingLoader {
	return &recordingLoader{inner: &MemoryLoader{Templates: map[string]string{
		"base.twig": traceBase,
		"mid.twig":  traceMid,
		"leaf.twig": traceLeaf,
	}}}
}

// TestThreeLevelNodeTreeAndLoadOrder proves the node/loader half of the
// inheritance chain before any output is produced: the leaf tree must
// carry an ExtendsNode pointing at mid.twig and contribute both its blocks
// into tree.Blocks(), and executing the leaf must load leaf, then mid,
// then base — parent trees are fetched lazily from the ModuleNode case in
// walk, in leaf-to-root order.
func TestThreeLevelNodeTreeAndLoadOrder(t *testing.T) {
	// Inspect the parsed node tree with a dedicated loader so the call log
	// below reflects the execution path alone.
	inspect := New(traceLoader())
	tree, err := inspect.load("leaf.twig")
	if err != nil {
		t.Fatalf("load leaf: %v", err)
	}
	root := tree.Root()
	if root.Parent == nil {
		t.Fatal("leaf tree has no Parent; expected an ExtendsNode")
	}
	if got := root.Parent.Tpl.(*parse.StringExpr).Text; got != "mid.twig" {
		t.Fatalf("leaf extends %q, want mid.twig", got)
	}
	blocks := tree.Blocks()
	if _, ok := blocks["a"]; !ok {
		t.Fatal(`leaf Blocks() missing overriding block "a"`)
	}
	if b, ok := blocks["b"]; !ok || b.Origin != "leaf.twig" {
		t.Fatalf(`leaf block b missing or wrong origin: %#v`, b)
	}

	loader := traceLoader()
	env := New(loader)
	if err := env.Execute("leaf.twig", io.Discard, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	wantCalls := []string{"leaf.twig", "mid.twig", "base.twig"}
	if strings.Join(loader.calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("loader call order = %v, want %v", loader.calls, wantCalls)
	}
}

// TestParentBlockVisibilityAtTwoDepths is the core block-scope invariant,
// run at BOTH inheritance depths so a fix that only handles one level
// cannot pass:
//
//   - depth 2 (leaf extends base): parent() inside leaf block "a" renders
//     base's definition; getBlock for a name absent from the child still
//     resolves to the base definition; a child-only block is invisible.
//   - depth 3 (leaf extends mid extends base): leaf's parent() renders
//     mid, and mid's own parent() (rendered recursively inside that
//     buffer) renders base. The pre-fix implementation resolved mid's
//     nested parent() back to mid and recursed into a stack overflow.
func TestParentBlockVisibilityAtTwoDepths(t *testing.T) {
	t.Run("depth 2", func(t *testing.T) {
		loader := &MemoryLoader{Templates: map[string]string{
			"base.twig": traceBase,
			"leaf.twig": `{% extends 'base.twig' %}{% block a %}LEAFA({{ parent() }}){% endblock %}{% block c %}CHILDC{% endblock %}`,
		}}
		out, err := runPlain(New(loader), "leaf.twig")
		if err != nil {
			t.Fatal(err)
		}
		// block a: child wins and parent body visible via parent();
		// block b: absent from the child, the base definition is visible;
		// block c: child-only, base has no slot, so it never renders.
		for _, want := range []string{"LEAFA(BASEA)", "BASEB"} {
			if !strings.Contains(out, want) {
				t.Errorf("output %q missing %q", out, want)
			}
		}
		if strings.Contains(out, "CHILDC") {
			t.Errorf("child-only block c leaked into output: %q", out)
		}
	})

	t.Run("depth 3", func(t *testing.T) {
		loader := traceLoader()
		out, err := runPlain(New(loader), "leaf.twig")
		if err != nil {
			t.Fatalf("depth-3 execution (parent chain): %v", err)
		}
		if want := "LEAFA(MIDA(BASEA))"; !strings.Contains(out, want) {
			t.Errorf("output %q missing nested chain %q", out, want)
		}
		// block b is overridden only in the leaf; base must render LEAFB.
		if !strings.Contains(out, "LEAFB") || strings.Contains(out, "BASEB") {
			t.Errorf("block b override visibility wrong: %q", out)
		}
	})
}

func runPlain(env *Env, name string) (string, error) {
	buf := &bytes.Buffer{}
	err := env.Execute(name, buf, nil)
	return buf.String(), err
}

// TestParentBufferOrdering inspects the Write event stream while the
// three-level chain renders. parent() renders into a private buffer and
// returns it as a value, so the surrounding static text of the child
// block must be emitted as its own writes rather than interleaving with
// the parent's body writes.
func TestParentBufferOrdering(t *testing.T) {
	loader := traceLoader()
	env := New(loader)
	w := &writeRecordingWriter{}
	if err := env.Execute("leaf.twig", w, nil); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(w.events, "")
	if want := "[base:LEAFA(MIDA(BASEA))|LEAFB]"; joined != want {
		t.Fatalf("joined events = %q, want %q", joined, want)
	}
	// Drop whitespace-only events; text/print adjacency decides how
	// TextNodes are split, not the buffering machinery.
	events := w.events[:0]
	for _, e := range w.events {
		if strings.TrimSpace(e) != "" {
			events = append(events, e)
		}
	}
	// The mid layer renders entirely into leaf's parent() buffer: the
	// complete "MIDA(BASEA)" value must reach the outer writer as a single
	// write produced by the PrintNode for {{ parent() }}, AFTER the leaf's
	// own "LEAFA(" text and before its ")" text. If the parent body were
	// walked directly against s.out instead of a swapped bytes.Buffer, its
	// BASEA write would interleave before LEAFA(.
	want := []string{"[base:", "LEAFA(", "MIDA(BASEA)", ")", "|", "LEAFB", "]"}
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Fatalf("non-whitespace write units = %v, want %v", events, want)
	}
}

// TestBlockStackStructureAfterExtends walks the executor's internal block
// stack directly. After extending mid from leaf, the stack must hold
// [rootTreeBlocks, parentTreeBlocks] with same-name entries in
// root-most-first order, which is the invariant getBlock and
// getParentBlockAfter rely on.
func TestBlockStackStructureAfterExtends(t *testing.T) {
	loader := traceLoader()
	env := New(loader)
	tree, err := env.load("leaf.twig")
	if err != nil {
		t.Fatal(err)
	}
	s := newState("leaf.twig", io.Discard, map[string]Value{}, env)
	s.blocks = append(s.blocks, tree.Blocks())
	// Simulate the ModuleNode parent walk: mid then base get appended.
	mid, err := env.load("mid.twig")
	if err != nil {
		t.Fatal(err)
	}
	s.blocks = append(s.blocks, mid.Blocks())
	base, err := env.load("base.twig")
	if err != nil {
		t.Fatal(err)
	}
	s.blocks = append(s.blocks, base.Blocks())

	leafA := s.blocks[0]["a"]
	midA := s.blocks[1]["a"]
	baseA := s.blocks[2]["a"]
	if leafA == nil || midA == nil || baseA == nil {
		t.Fatalf("missing block definitions on stack: %v", s.blocks)
	}
	if got := s.getBlock("a"); got != leafA {
		t.Errorf("getBlock(a) = %v, want leaf definition (first override wins)", got.Origin)
	}
	if got := s.getParentBlockAfter(leafA, "a"); got != midA {
		t.Errorf("parent of leaf a = %v, want mid", originOf(got))
	}
	if got := s.getParentBlockAfter(midA, "a"); got != baseA {
		t.Errorf("parent of mid a = %v, want base", originOf(got))
	}
	if got := s.getParentBlockAfter(baseA, "a"); got != nil {
		t.Errorf("parent of base a = %v, want nil (top of chain)", originOf(got))
	}
}

func originOf(b *parse.BlockNode) string {
	if b == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s block %s", b.Origin, b.Name)
}
