// Command schema-dump regenerates schema.sql from a live Postgres for
// sqlc to consume. The Makefile's schema-dump target used to be a shell
// recipe (psql preflight + go run ./cmd/migrate + pg_dump -s + sed
// noise filter); this binary owns the same flow in Go so the failure
// modes live behind os/exec and the regex noise filter is
// compile-checked and unit-testable.
//
// schema.sql is the single source-of-truth schema file sqlc consumes
// (issue #125, ADR-017). sqlc v1.27.0 does not merge
// `create table if not exists` statements across multiple migration
// files, so pointing sqlc at migrations/ would diverge from the live
// schema wherever a migration adds columns to an existing table.
// Instead, schema-dump applies the full migration set against a
// reachable Postgres, runs pg_dump -s, strips the version-noise lines
// pg_dump emits (which can change with every Postgres minor version),
// and writes the deterministic output to schema.sql. The sqlc-check
// CI gate then diffs the regenerated pkg/state/sqlc/* against the
// committed baseline, which transitively proves schema.sql is in sync
// with the migration set.
//
// Usage:
//
//	DATABASE_URL=postgres://... schema-dump           # writes ./schema.sql
//	DATABASE_URL=postgres://... schema-dump -o other   # writes ./other
//
// Exit code 0 on success, 1 on any failure. Failures print to stderr
// with the failing operation's name so CI surfaces the diagnostic
// cleanly. Idempotent: re-running against the same migration set
// produces byte-identical output (verified by the deterministic
// pg_dump -s output against an unchanged schema).
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/onebox-faas/faas/pkg/db"
)

const defaultOutput = "schema.sql"

// pgDumpNoise are pg_dump -s banner lines that change between
// Postgres minor versions and would defeat a textual diff. Compiled
// once at init; missing a new noise line is loud (a stale
// schema.sql in CI), which is the failure mode we want — never
// silent drift detection loss. Each pattern matches the full line
// (including the trailing newline); (?m) makes ^ match each line
// start, not the input start, so a banner that appears mid-file is
// also caught.
var pgDumpNoise = []*regexp.Regexp{
	// \restrict <token> and \unrestrict <token> are psql meta-commands
	// pg_dump 16+ emits at the top of the dump. The token is random
	// per dump, so a textual diff would always fail without this.
	regexp.MustCompile(`(?m)^\\(restrict|unrestrict) [^\n]*\n?`),
	regexp.MustCompile(`(?m)^-- Dumped from database version [^\n]*\n?`),
	regexp.MustCompile(`(?m)^-- Dumped by pg_dump version [^\n]*\n?`),
	regexp.MustCompile(`(?m)^-- PostgreSQL database dump complete[^\n]*\n?`),
}

func main() {
	outPath := flag.String("o", defaultOutput, "output path (default ./schema.sql)")
	flag.Parse()

	if err := run(*outPath); err != nil {
		fmt.Fprintf(os.Stderr, "schema-dump: %v\n", err)
		os.Exit(1)
	}
}

func run(outPath string) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1. Apply migrations so the live schema matches HEAD.
	pool, err := db.Open(ctx, "")
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer pool.Close()
	if err := db.MigrateUp(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.Info("migrations applied")

	// 2. Shell to pg_dump -s. We don't reimplement the schema-dump
	// renderer in Go because the canonical format is pg_dump's, and
	// every Postgres minor version can change it. Failure modes
	// (pg_dump missing, DSN invalid) are owned by os/exec.
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL not set")
	}
	dump, err := exec.CommandContext(ctx, "pg_dump",
		"-s", "--no-owner", "--no-privileges",
		"--no-sync", "--no-tablespaces", dsn,
	).Output()
	if err != nil {
		return fmt.Errorf("pg_dump: %w", err)
	}

	// 3. Strip pg_dump version noise so the diff is stable across
	// Postgres minor versions.
	filtered := stripNoise(dump)

	if err := os.WriteFile(outPath, filtered, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	lines := bytes.Count(filtered, []byte{'\n'}) + 1
	fmt.Fprintf(os.Stderr, "schema-dump: %s regenerated (%d lines)\n", outPath, lines)
	return nil
}

// stripNoise removes the pg_dump -s banner lines that change between
// Postgres minor versions. Exposed at package scope for testability
// (cmd/schema-dump/main_test.go::TestStripNoise).
func stripNoise(in []byte) []byte {
	out := in
	for _, re := range pgDumpNoise {
		out = re.ReplaceAll(out, nil)
	}
	return out
}
