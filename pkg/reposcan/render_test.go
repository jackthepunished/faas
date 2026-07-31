package reposcan

import (
	"sort"
	"testing"
	"testing/fstest"
)

func TestDetectRender_WebWorkerAndCronJobs(t *testing.T) {
	t.Parallel()
	body := `services:
  - name: api
    type: web
  - name: indexer
    type: worker
  - name: night-batch
    type: worker
cronJobs:
  - name: hourly
    schedule: "0 * * * *"
  - name: nightly
    schedule: "0 3 * * *"
`
	fsys := fstest.MapFS{
		"render.yaml": &fstest.MapFile{Data: []byte(body)},
	}
	seeds, _, _, err := detectRender(fsys)
	if err != nil {
		t.Fatalf("detectRender: %v", err)
	}
	byName := map[string]workloadSeed{}
	for _, s := range seeds {
		byName[s.name] = s
	}
	want := map[string]Class{
		"api":         ClassHTTP,
		"indexer":     ClassWorker,
		"night-batch": ClassWorker,
		"hourly":      ClassJob,
		"nightly":     ClassJob,
	}
	if len(byName) != len(want) {
		got := make([]string, 0, len(byName))
		for k := range byName {
			got = append(got, k)
		}
		sort.Strings(got)
		t.Errorf("seed count = %d (%v), want %d (%v)", len(byName), got, len(want), keysSorted(want))
	}
	for n, wantClass := range want {
		s, ok := byName[n]
		if !ok {
			t.Errorf("seed %q missing", n)
			continue
		}
		if s.class != wantClass {
			t.Errorf("seed %q class = %q, want %q", n, s.class, wantClass)
		}
	}
	if byName["hourly"].schedule != "0 * * * *" {
		t.Errorf("hourly schedule = %q", byName["hourly"].schedule)
	}
}

func TestDetectRender_AbsentFile(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{"Dockerfile": &fstest.MapFile{Data: []byte("FROM scratch")}}
	seeds, _, _, err := detectRender(fsys)
	if err != nil {
		t.Fatalf("detectRender: %v", err)
	}
	if len(seeds) != 0 {
		t.Errorf("seeds = %v, want empty", names(seeds))
	}
}

func keysSorted(m map[string]Class) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
