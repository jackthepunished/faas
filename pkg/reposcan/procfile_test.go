package reposcan

import (
	"testing"
	"testing/fstest"
)

// TestDetectProcfile_ClassMapping covers the canonical Procfile
// process-type set: web → http, worker → worker, cron/clock/
// scheduler → job, release → skip, custom → unknown but
// accepted. Each line must produce exactly one workload with the
// expected class.
func TestDetectProcfile_ClassMapping(t *testing.T) {
	t.Parallel()
	body := `web: bundle exec rails s
worker: bundle exec sidekiq
cron: bundle exec nightly
clock: bundle exec scheduler
scheduler: bundle exec scheduler
release: bundle exec rake deploy
custom: bundle exec widget
`
	fsys := fstest.MapFS{
		"Procfile": &fstest.MapFile{Data: []byte(body)},
	}
	seeds, _, warnings, err := detectProcfile(fsys)
	if err != nil {
		t.Fatalf("detectProcfile: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	byClass := map[string]Class{}
	for _, s := range seeds {
		byClass[s.name] = s.class
	}
	want := map[string]Class{
		"web":       ClassHTTP,
		"worker":    ClassWorker,
		"cron":      ClassJob,
		"clock":     ClassJob,
		"scheduler": ClassJob,
		"custom":    ClassUnknown,
	}
	for proc, wantClass := range want {
		got, ok := byClass[proc]
		if !ok {
			t.Errorf("proc %q missing from seeds", proc)
			continue
		}
		if got != wantClass {
			t.Errorf("proc %q class = %q, want %q", proc, got, wantClass)
		}
	}
	if _, dropped := byClass["release"]; dropped {
		t.Errorf("release: should be skipped, found in seeds %v", byClass)
	}
	if len(seeds) != len(want) {
		t.Errorf("seed count = %d, want %d (release: excluded)", len(seeds), len(want))
	}
}

// TestDetectProcfile_WebCommandCarried — the workload's Command
// carries the full RHS of the Procfile line. The merge rule
// later pairs this with a compose `web`'s command when both
// fire on (RootDir="", Name="web").
func TestDetectProcfile_WebCommandCarried(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"Procfile": &fstest.MapFile{Data: []byte("web: bundle exec rails s -p 3000\n")},
	}
	seeds, _, _, err := detectProcfile(fsys)
	if err != nil {
		t.Fatalf("detectProcfile: %v", err)
	}
	if len(seeds) != 1 {
		t.Fatalf("want 1 seed, got %d", len(seeds))
	}
	if len(seeds[0].command) != 1 || seeds[0].command[0] != "bundle exec rails s -p 3000" {
		t.Errorf("command = %v, want [bundle exec rails s -p 3000]", seeds[0].command)
	}
}

// TestDetectProcfile_AbsentFile — quiet skip when no Procfile.
func TestDetectProcfile_AbsentFile(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{"Dockerfile": &fstest.MapFile{Data: []byte("FROM scratch")}}
	seeds, _, warnings, err := detectProcfile(fsys)
	if err != nil {
		t.Fatalf("detectProcfile: %v", err)
	}
	if len(seeds) != 0 || len(warnings) != 0 {
		t.Errorf("detectProcfile = (%v, %v), all want empty",
			names(seeds), warnings)
	}
}

// TestDetectProcfile_CommentsAndBlankLinesIgnored — only payload
// lines become workloads; comments and blanks are skipped.
func TestDetectProcfile_CommentsAndBlankLinesIgnored(t *testing.T) {
	t.Parallel()
	body := `
# this is a comment
web: bundle exec rails s

# another comment
worker: bundle exec sidekiq

`
	fsys := fstest.MapFS{"Procfile": &fstest.MapFile{Data: []byte(body)}}
	seeds, _, _, err := detectProcfile(fsys)
	if err != nil {
		t.Fatalf("detectProcfile: %v", err)
	}
	if got := names(seeds); !equalSet(got, []string{"web", "worker"}) {
		t.Errorf("seed names = %v, want {web,worker}", got)
	}
}
