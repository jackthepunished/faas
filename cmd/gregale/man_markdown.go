// man_markdown.go — `gregale man --markdown`.
//
// Renders the whole command manifest (cli_meta.go) as one Markdown
// document. The public web app vendors the committed output verbatim
// (faas-web content/docs/cli-reference.md), so the binary stays the
// single source of truth for the reference the way it already is for
// man pages and shell completion. TestMarkdownReferenceFresh keeps
// docs/cli-reference.md in lock-step with the manifest.

package main

import (
	"fmt"
	"io"
	"strings"
)

// renderMarkdownReference emits the manifest as Markdown. Headings are
// stable anchors: `## <command>` and `### <command> <sub>`.
func renderMarkdownReference(w io.Writer, cmds []cliCommand) {
	fmt.Fprintln(w, "# gregale CLI reference")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Generated from the CLI's command manifest by `gregale man --markdown`. Do not edit by hand.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Command | What it does |")
	fmt.Fprintln(w, "|---|---|")
	for _, c := range cmds {
		fmt.Fprintf(w, "| [`%s`](#%s) | %s |\n", c.Name, c.Name, mdCell(c.Short))
	}
	for _, c := range cmds {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "## %s\n\n", c.Name)
		fmt.Fprintf(w, "%s\n\n", c.Short)
		fmt.Fprintf(w, "`%s`\n\n", mdSynopsis(c))
		if len(c.ClosedSet) > 0 && len(c.Positionals) > 0 {
			fmt.Fprintf(w, "%s is one of %s.\n\n", c.Positionals[0], mdCodeList(c.ClosedSet))
		}
		if len(c.Flags) > 0 {
			mdFlagTable(w, c.Flags)
		}
		for _, s := range c.Subcommands {
			fmt.Fprintf(w, "### %s %s\n\n%s\n\n", c.Name, s.Name, s.Short)
			if len(s.Flags) > 0 {
				mdFlagTable(w, s.Flags)
			}
		}
	}
}

func mdSynopsis(c cliCommand) string {
	parts := []string{"gregale", c.Name}
	if len(c.Subcommands) > 0 {
		parts = append(parts, "<subcommand>")
	}
	parts = append(parts, c.Positionals...)
	for _, f := range c.Flags {
		if f.Req {
			parts = append(parts, "--"+f.Name+" <value>")
		} else {
			parts = append(parts, "[--"+f.Name+"]")
		}
	}
	return strings.Join(parts, " ")
}

func mdFlagTable(w io.Writer, flags []cliFlag) {
	fmt.Fprintln(w, "| Flag | Meaning | |")
	fmt.Fprintln(w, "|---|---|---|")
	for _, f := range flags {
		extra := ""
		if f.Req {
			extra = "required"
		}
		if len(f.ClosedSet) > 0 {
			if extra != "" {
				extra += "; "
			}
			extra += "one of " + mdCodeList(f.ClosedSet)
		}
		fmt.Fprintf(w, "| `--%s` | %s | %s |\n", f.Name, mdCell(f.Short), extra)
	}
	fmt.Fprintln(w)
}

func mdCodeList(vals []string) string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = "`" + v + "`"
	}
	return strings.Join(out, " · ")
}

// mdCell keeps a table row on one line and escapes the column separator.
func mdCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "|", "\\|")
}
