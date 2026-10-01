package cliutil

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestVersionLine(t *testing.T) {
	if got := VersionLine("stretchstore"); got != "stretchstore "+Version {
		t.Fatalf("got %q", got)
	}
}

func TestCompletionAllShells(t *testing.T) {
	cmds := []string{"run", "stats", "clear", "test"}
	for _, sh := range []string{"bash", "zsh", "fish"} {
		out, err := Completion("stretchcpu", cmds, sh)
		if err != nil {
			t.Fatalf("%s: %v", sh, err)
		}
		for _, c := range cmds {
			if !strings.Contains(out, c) {
				t.Fatalf("%s: missing command %q in:\n%s", sh, c, out)
			}
		}
		if !strings.Contains(out, "stretchcpu") {
			t.Fatalf("%s: missing tool name", sh)
		}
	}
	if _, err := Completion("x", cmds, "powershell"); err == nil {
		t.Fatal("expected error for unknown shell")
	}
}

func TestProgress(t *testing.T) {
	var b bytes.Buffer
	p := NewProgress(&b, "fill", time.Hour) // interval huge: only Done prints
	p.Add(1 << 20)
	p.Add(1 << 20)
	if b.Len() != 0 {
		t.Fatalf("expected no interim lines, got %q", b.String())
	}
	p.Done()
	out := b.String()
	if !strings.Contains(out, "[fill]") || !strings.Contains(out, "2.00 MB") {
		t.Fatalf("bad Done line: %q", out)
	}
	p.Done() // idempotent
	if n := strings.Count(b.String(), "[fill]"); n != 1 {
		t.Fatalf("Done printed %d times", n)
	}
}

func TestProgressInterim(t *testing.T) {
	var b bytes.Buffer
	p := NewProgress(&b, "fill", 0) // every Add prints
	p.Add(1 << 20)
	if !strings.Contains(b.String(), "so far") {
		t.Fatalf("expected interim line, got %q", b.String())
	}
}
