package stick

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// failLoader returns sentinel for every name. It lets the test distinguish
// "template not found" (swallowable by `ignore missing`) from any other
// loader failure (must propagate with cause intact).
type failLoader struct {
	sentinel error
	calls    []string
}

func (l *failLoader) Load(name string) (Template, error) {
	l.calls = append(l.calls, name)
	return nil, fmt.Errorf("custom loader refused %q: %w", name, l.sentinel)
}

// TestLoaderErrorKeepsPathAndCause exercises every executor path that
// resolves a template name. The error reaching the caller must carry both
// the exact template name and the original cause (errors.Is must find
// os.ErrNotExist), instead of a bare "file does not exist".
func TestLoaderErrorKeepsPathAndCause(t *testing.T) {
	t.Run("include missing", func(t *testing.T) {
		env := New(&MemoryLoader{Templates: map[string]string{
			"main": `{% include 'partial.twig' %}`,
		}})
		err := env.Execute("main", io.Discard, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "partial.twig") {
			t.Errorf("error %q lost the template path", err.Error())
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error %v does not wrap os.ErrNotExist", err)
		}
	})

	t.Run("extends missing", func(t *testing.T) {
		env := New(&MemoryLoader{Templates: map[string]string{
			"leaf": `{% extends 'ghost.twig' %}`,
		}})
		err := env.Execute("leaf", io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), "ghost.twig") {
			t.Fatalf("expected error naming ghost.twig, got %v", err)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("extends error %v does not wrap os.ErrNotExist", err)
		}
	})

	t.Run("import missing", func(t *testing.T) {
		env := New(&MemoryLoader{Templates: map[string]string{
			"main": `{% import 'macrolib.twig' as ui %}`,
		}})
		err := env.Execute("main", io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), "macrolib.twig") {
			t.Fatalf("expected error naming macrolib.twig, got %v", err)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("import error %v does not wrap os.ErrNotExist", err)
		}
	})

	t.Run("entry template missing", func(t *testing.T) {
		env := New(&MemoryLoader{Templates: map[string]string{}})
		err := env.Execute("entry.twig", io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), "entry.twig") {
			t.Fatalf("expected error naming entry.twig, got %v", err)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("entry error %v does not wrap os.ErrNotExist", err)
		}
	})
}

// TestIgnoreMissingSwallowsOnlyNotExist is the easily-guessed-wrong
// invariant: `ignore missing` mirrors PHP-Twig and hides only absent
// templates. A loader failing for another reason must surface immediately
// — with its cause — for both single-name and candidate-array includes,
// and before any subsequent candidates are probed.
func TestIgnoreMissingSwallowsOnlyNotExist(t *testing.T) {
	t.Run("not exist is swallowed", func(t *testing.T) {
		env := New(&MemoryLoader{Templates: map[string]string{
			"main": `[{% include 'absent.twig' ignore missing %}]`,
		}})
		buf := &bytes.Buffer{}
		if err := env.Execute("main", buf, nil); err != nil {
			t.Fatalf("ignore missing should hide not-exist, got %v", err)
		}
		if buf.String() != "[]" {
			t.Errorf("got %q, want []", buf.String())
		}
	})

	t.Run("other loader error propagates single name", func(t *testing.T) {
		loader := &failLoader{sentinel: os.ErrPermission}
		env := New(&MemoryLoader{Templates: map[string]string{
			"main": `{% include 'secrets.twig' ignore missing %}`,
		}})
		// main must come from the strict in-memory set, while the included
		// name is routed through the failing loader — achieved by a
		// composing loader.
		env.Loader = &composedLoader{
			primary: env.Loader,
			failFor: loader,
		}
		err := env.Execute("main", io.Discard, nil)
		if err == nil {
			t.Fatal("expected permission error, got nil")
		}
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("error %v lost original cause os.ErrPermission", err)
		}
		if !strings.Contains(err.Error(), "secrets.twig") {
			t.Errorf("error %q lost candidate path", err.Error())
		}
		if strings.Join(loader.calls, ",") != "secrets.twig" {
			t.Fatalf("loader calls = %v; structural failure must stop the candidate loop", loader.calls)
		}
	})

	t.Run("other loader error propagates from candidate array", func(t *testing.T) {
		loader := &failLoader{sentinel: errors.New("disk offline")}
		env := New(&MemoryLoader{Templates: map[string]string{
			"main": `{% include ['first.twig', 'second.twig'] ignore missing %}`,
		}})
		env.Loader = &composedLoader{primary: env.Loader, failFor: loader}
		err := env.Execute("main", io.Discard, nil)
		if err == nil {
			t.Fatal("expected propagated disk error, got nil")
		}
		if !strings.Contains(err.Error(), "disk offline") || !strings.Contains(err.Error(), "first.twig") {
			t.Errorf("error %q missing cause or candidate", err.Error())
		}
		if strings.Join(loader.calls, ",") != "first.twig" {
			t.Fatalf("loader calls = %v; execution must abort on first structural error", loader.calls)
		}
	})
}

// composedLoader serves known names from primary and routes every other
// name through failFor. It avoids any special-casing on template names in
// the executor: routing is expressed purely through the Loader contract.
type composedLoader struct {
	primary Loader
	failFor *failLoader
}

func (l *composedLoader) Load(name string) (Template, error) {
	if tpl, err := l.primary.Load(name); err == nil {
		return tpl, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return l.failFor.Load(name)
}

// TestParentParseErrorKeepsName ensures a parse failure reached through
// extends is attributed to the PARENT template's name and position, not to
// the leaf being executed.
func TestParentParseErrorKeepsName(t *testing.T) {
	env := New(&MemoryLoader{Templates: map[string]string{
		"leaf.twig":   `{% extends 'broken.twig' %}`,
		"broken.twig": "fine text\n\n{% block 42 %}",
	}})
	err := env.Execute("leaf.twig", io.Discard, nil)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "broken.twig") {
		t.Errorf("error %q does not identify the parent template", err.Error())
	}
	if strings.Contains(err.Error(), "leaf.twig") {
		t.Errorf("error %q wrongly attributes failure to the leaf", err.Error())
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error %q lost exact line info", err.Error())
	}
}
