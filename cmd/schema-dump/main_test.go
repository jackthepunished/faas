package main

import (
	"strings"
	"testing"
)

func TestStripNoise(t *testing.T) {
	in := strings.Join([]string{
		"\\restrict abc123",
		"-- Dumped from database version 16.2",
		"-- Dumped by pg_dump version 16.2",
		"CREATE TABLE public.crons (",
		"    id uuid NOT NULL,",
		");",
		"-- PostgreSQL database dump complete",
		"",
	}, "\n")

	got := string(stripNoise([]byte(in)))
	for _, banned := range []string{
		"\\restrict",
		"Dumped from database version",
		"Dumped by pg_dump version",
		"PostgreSQL database dump complete",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("stripNoise left %q in output:\n%s", banned, got)
		}
	}
	if !strings.Contains(got, "CREATE TABLE public.crons") {
		t.Errorf("stripNoise removed the real schema content:\n%s", got)
	}
}

// TestStripNoise_PreservesUnrelated ensures the noise filter is
// surgical: a `-- PostgreSQL` comment in a function body must survive.
func TestStripNoise_PreservesUnrelated(t *testing.T) {
	in := "-- PostgreSQL is the database\n-- Dumped by pg_dump version 16.2\n"
	got := string(stripNoise([]byte(in)))
	if !strings.Contains(got, "-- PostgreSQL is the database") {
		t.Errorf("stripNoise over-filtered; lost the unrelated -- PostgreSQL comment:\n%s", got)
	}
	if strings.Contains(got, "Dumped by pg_dump") {
		t.Errorf("stripNoise left pg_dump version line in output:\n%s", got)
	}
}
