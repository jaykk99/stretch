// Package cliutil holds the small shared UX pieces for the four Stretch
// CLIs: the build version, shell-completion scripts, and a log-friendly
// progress reporter. No external dependencies.
package cliutil

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"stretchstore/internal/size"
)

// Version is the release version printed by `<tool> version`.
const Version = "0.2.0"

// VersionLine renders e.g. "stretchstore 0.2.0".
func VersionLine(tool string) string {
	return fmt.Sprintf("%s %s", tool, Version)
}

// Completion returns a shell completion script for tool's subcommands.
// shell is one of "bash", "zsh", "fish".
func Completion(tool string, commands []string, shell string) (string, error) {
	cmds := append([]string(nil), commands...)
	sort.Strings(cmds)
	quoted := make([]string, len(cmds))
	for i, c := range cmds {
		quoted[i] = c
	}
	list := strings.Join(quoted, " ")
	switch shell {
	case "bash":
		return fmt.Sprintf(`# %[1]s bash completion — install to /etc/bash_completion.d/ or source it.
_%[1]s_complete() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    if [ "$COMP_CWORD" -eq 1 ]; then
        COMPREPLY=($(compgen -W "%[2]s" -- "$cur"))
    fi
}
complete -F _%[1]s_complete %[1]s
`, tool, list), nil
	case "zsh":
		return fmt.Sprintf(`#compdef %[1]s
# %[1]s zsh completion — drop in a directory on your $fpath.
_%[1]s() {
    local -a cmds
    cmds=(%[2]s)
    _describe 'command' cmds
}
_%[1]s "$@"
`, tool, list), nil
	case "fish":
		var b strings.Builder
		fmt.Fprintf(&b, "# %s fish completion — put in ~/.config/fish/completions/%s.fish\n", tool, tool)
		for _, c := range cmds {
			fmt.Fprintf(&b, "complete -c %s -f -n '__fish_use_subcommand' -a %s\n", tool, c)
		}
		return b.String(), nil
	default:
		return "", fmt.Errorf("unknown shell %q: want bash, zsh, or fish", shell)
	}
}

// Progress reports long-running work as periodic log lines, e.g.
//
//	[fill] 320.0 MB written so far...
//	[fill] done: 1.0 GB in 42s
//
// Lines (not \r rewrites) so piped output and logs stay readable.
type Progress struct {
	w        io.Writer
	label    string
	interval time.Duration
	last     time.Time
	start    time.Time
	done     uint64
	finished bool
}

// NewProgress starts a progress reporter. interval is how often "so far"
// lines print; the final Done line always prints.
func NewProgress(w io.Writer, label string, interval time.Duration) *Progress {
	now := time.Now()
	return &Progress{w: w, label: label, interval: interval, last: now, start: now}
}

// Add records n more bytes of work and prints a "so far" line at most
// every interval.
func (p *Progress) Add(n uint64) {
	if p.finished {
		return
	}
	p.done += n
	now := time.Now()
	if now.Sub(p.last) >= p.interval {
		p.last = now
		fmt.Fprintf(p.w, "  [%s] %s so far...\n", p.label, size.Format(uint64(p.done)))
	}
}

// Done prints the final line.
func (p *Progress) Done() {
	if p.finished {
		return
	}
	p.finished = true
	fmt.Fprintf(p.w, "  [%s] done: %s in %s\n", p.label, size.Format(uint64(p.done)), time.Since(p.start).Round(time.Second))
}
