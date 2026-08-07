// commands/man.go — Tier A8 / ADR-083.
//
// `gregale man` (full gregale(1) page) and `gregale man <command>`
// (per-command gregale-<command>(1)) emit roff source to stdout.
//
// Output format is groff man (the same flavour `man` renders on
// Linux + macOS). The script can pipe the result to `man -l -`
// for immediate rendering, or redirect to a file under
// /usr/local/share/man/man1/ for permanent installation.
//
// The roff emits NAME / SYNOPSIS / DESCRIPTION / COMMANDS /
// EXAMPLES / SEE ALSO sections per the Linux man-pages(7)
// convention. Headers are bracketed in .SH; the synopsis uses
// .B for the literal tokens; URLs are .UR + .UE.
//
// Output is human-only (never takes --json) — the roff itself
// IS the structured format. A `--json` flag would require
// defining an alternate schema just for this command, which is
// not worth the surface.

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const manDocsTopic = "man"

// gregaleVersion is read from wire.Version at startup via
// initGregaleVersion() (set in main.go via a tiny init() or
// assigned at process boot — wire.Version is a string constant
// already, so this is just an indirection so tests can swap it).
var gregaleVersion = "dev"

// cmdMan dispatches `gregale man [command]`. With no arg, prints
// the top-level gregale(1) page; with one arg, prints the
// per-command page gregale-<command>(1).
func cmdMan(args []string) int {
	switch len(args) {
	case 0:
		renderManTop(osStdout)
		return 0
	case 1:
		cmd, ok := lookupCliCommand(args[0])
		if !ok {
			fmt.Fprintf(os.Stderr, "gregale man: unknown command %q\n", args[0])
			return 1
		}
		renderManCommand(osStdout, cmd)
		return 0
	default:
		PrintUsage(os.Stderr, "usage: gregale man [<command>]", manDocsTopic)
		return 1
	}
}

// lookupCliCommand returns the cliCommand for name. Linear scan
// is fine — the manifest has ~50 entries.
func lookupCliCommand(name string) (cliCommand, bool) {
	for _, c := range cliCommands {
		if c.Name == name {
			return c, true
		}
	}
	return cliCommand{}, false
}

// renderManTop writes the gregale(1) top-level page.
func renderManTop(w io.Writer) {
	manHeader(w, "GREGALE(1)", "gregale Manual", "gregale")
	manSection(w, "NAME", func(w io.Writer) {
		fmt.Fprintln(w, ".B gregale")
		fmt.Fprintln(w, "\\- deploy apps and functions that scale to zero")
	})
	manSection(w, "SYNOPSIS", func(w io.Writer) {
		fmt.Fprintln(w, ".B gregale")
		fmt.Fprintln(w, ".RI [ command ]")
		fmt.Fprintln(w, ".RI [ flags ]")
	})
	manSection(w, "DESCRIPTION", func(w io.Writer) {
		fmt.Fprintln(w, `.PP`)
		fmt.Fprintln(w, `gregale is the customer-facing CLI for the Gregale FaaS platform.`)
		fmt.Fprintln(w, `It is the primary interface to the platform; every action the platform`)
		fmt.Fprintln(w, `supports is reachable from this single binary.`)
	})
	manSection(w, "COMMANDS", func(w io.Writer) {
		fmt.Fprintln(w, `.PP`)
		fmt.Fprintln(w, `Run \fBgregale help\fP for the full command list. The most common verbs:`)
		fmt.Fprintln(w, ".TP")
		fmt.Fprintln(w, ".BR apps ,", " \\fIalerts\\fP,")
		fmt.Fprintln(w, ".BR deployments ,", " \\fIregistry\\fP,")
		fmt.Fprintln(w, ".BR webhooks ,", " \\fIinvocations\\fP,")
		fmt.Fprintln(w, ".BR crons ,", " \\fIdelayed-task\\fP,")
		fmt.Fprintln(w, ".BR orgs ,", " \\fIkeys\\fP,")
		fmt.Fprintln(w, ".BR mfa")
	})
	manSection(w, "GLOBAL FLAGS", func(w io.Writer) {
		fmt.Fprintln(w, ".TP")
		fmt.Fprintln(w, ".BR \\-\\-json")
		fmt.Fprintln(w, `Machine-readable output. Equivalent to`)
		fmt.Fprintln(w, `.B FAAS_JSON=1`)
		fmt.Fprintln(w, `in the environment.`)
	})
	manSection(w, "EXAMPLES", func(w io.Writer) {
		fmt.Fprintln(w, `.PP`)
		fmt.Fprintln(w, `List your apps:`)
		fmt.Fprintln(w, `.PP`)
		fmt.Fprintln(w, ".RS 4")
		fmt.Fprintln(w, `.nf`)
		fmt.Fprintln(w, `gregale apps`)
		fmt.Fprintln(w, `.fi`)
		fmt.Fprintln(w, ".RE")
		fmt.Fprintln(w, `.PP`)
		fmt.Fprintln(w, `Deploy from a tarball:`)
		fmt.Fprintln(w, ".RS 4")
		fmt.Fprintln(w, ".nf")
		fmt.Fprintln(w, "gregale deploy --tarball ./app.tar.gz --app my-app")
		fmt.Fprintln(w, ".fi")
		fmt.Fprintln(w, ".RE")
	})
	manSection(w, "SEE ALSO", func(w io.Writer) {
		fmt.Fprintf(w, ".UR %scompletion\n", docsURLBase)
		fmt.Fprintln(w, "gregale completion (bash|zsh|fish|powershell)")
		fmt.Fprintln(w, ".UE")
		fmt.Fprintln(w, ".PP")
		fmt.Fprintf(w, ".UR %s\n", docsURLBase)
		fmt.Fprintln(w, "gregale docs")
		fmt.Fprintln(w, ".UE")
	})
	manFooter(w)
}

// renderManCommand writes the gregale-<command>(1) per-command page.
func renderManCommand(w io.Writer, c cliCommand) {
	manHeader(w, "GREGALE-"+strings.ToUpper(c.Name)+"(1)", "gregale "+c.Name+" Manual", "gregale-"+c.Name)
	manSection(w, "NAME", func(w io.Writer) {
		fmt.Fprintf(w, ".B gregale-%s\n", c.Name)
		fmt.Fprintf(w, "\\- %s\n", escapeRoff(c.Short))
	})
	manSection(w, "SYNOPSIS", func(w io.Writer) {
		fmt.Fprintf(w, ".B gregale %s\n", c.Name)
		for _, s := range c.Subcommands {
			fmt.Fprintf(w, ".RI [ %s ]\n", s.Name)
		}
		for _, p := range c.Positionals {
			fmt.Fprintf(w, ".RI %s\n", p)
		}
		for _, f := range c.Flags {
			if f.Req {
				fmt.Fprintf(w, ".RI [ --%s ", f.Name)
				fmt.Fprint(w, ".IR value ")
				fmt.Fprint(w, "]\n")
			} else {
				fmt.Fprintf(w, ".RI [ --%s ", f.Name)
				fmt.Fprint(w, ".IR value ")
				fmt.Fprint(w, "]\n")
			}
		}
	})
	manSection(w, "DESCRIPTION", func(w io.Writer) {
		fmt.Fprintf(w, ".PP\n%s\n", escapeRoff(c.Short))
	})
	if len(c.Subcommands) > 0 {
		manSection(w, "SUBCOMMANDS", func(w io.Writer) {
			fmt.Fprintln(w, ".TP")
			for _, s := range c.Subcommands {
				fmt.Fprintf(w, ".BR %s\n", s.Name)
				fmt.Fprintf(w, "%s\n", escapeRoff(s.Short))
				fmt.Fprintln(w, ".TP")
			}
		})
	}
	if len(c.Flags) > 0 {
		manSection(w, "FLAGS", func(w io.Writer) {
			fmt.Fprintln(w, ".TP")
			for _, f := range c.Flags {
				fmt.Fprintf(w, ".BR --%s\n", f.Name)
				fmt.Fprintf(w, "%s\n", escapeRoff(f.Short))
				if len(f.ClosedSet) > 0 {
					fmt.Fprintf(w, "Allowed values: %s.\n", strings.Join(f.ClosedSet, ", "))
				}
				fmt.Fprintln(w, ".TP")
			}
		})
	}
	manSection(w, "SEE ALSO", func(w io.Writer) {
		fmt.Fprintf(w, ".UR %s%s\n", docsURLBase, c.DocSlug)
		fmt.Fprintf(w, "gregale %s (docs)\n", c.Name)
		fmt.Fprintln(w, ".UE")
		fmt.Fprintln(w, ".PP")
		fmt.Fprintf(w, ".UR %s\n", docsURLBase)
		fmt.Fprintln(w, "gregale(1) top-level manual")
		fmt.Fprintln(w, ".UE")
	})
	manFooter(w)
}

// manHeader writes the page preamble only: .TH title section date source
// manual. The NAME section is rendered by renderManTop/renderManCommand
// via manSection("NAME", ...) — keeping all .SH openings in one place.
func manHeader(w io.Writer, title, subtitle, source string) {
	fmt.Fprintf(w, ".TH %s 1 \"%s\" \"%s\"\n", title, gregaleVersion, strings.ToUpper(source))
	_ = subtitle // subtitle is rendered inside the NAME section body
}

// manSection writes a section header followed by the body callback.
// The body callback receives w and emits the roff for the section.
func manSection(w io.Writer, name string, body func(w io.Writer)) {
	fmt.Fprintf(w, ".SH %s\n", strings.ToUpper(name))
	body(w)
}

// manFooter writes the trailing blank line + end-of-file marker.
// Most renderers add a final newline so the file ends cleanly
// regardless of how the user pipes the output.
func manFooter(w io.Writer) {
	fmt.Fprintln(w)
}

// escapeRoff backslash-escapes roff-significant characters. The
// common ones are backslash itself and the period at the start
// of a line (which would otherwise be interpreted as a macro).
// Inside .SH and .TP sections the period is harmless; we only
// escape when text is part of a paragraph.
func escapeRoff(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\\", "\\\\")
	// Period-only escaping: only the first character of each line.
	// For our use (single-line summaries), we just protect a leading
	// period if any.
	if strings.HasPrefix(s, ".") {
		s = "\\&" + s
	}
	return s
}
