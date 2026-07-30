package reposcan

import (
	"testing"
	"testing/fstest"
)

func TestDetectFly_AppAndProcesses(t *testing.T) {
	t.Parallel()
	body := `app = "my-fly-app"

[processes]
web = 1
worker = 1
`
	fsys := fstest.MapFS{
		"fly.toml": &fstest.MapFile{Data: []byte(body)},
	}
	seeds, _, _, err := detectFly(fsys)
	if err != nil {
		t.Fatalf("detectFly: %v", err)
	}
	byName := map[string]workloadSeed{}
	for _, s := range seeds {
		byName[s.name] = s
	}
	app, ok := byName["my-fly-app"]
	if !ok || app.class != ClassHTTP {
		t.Errorf("app seed = (%v, %s); want my-fly-app/http", ok, app.class)
	}
	if _, ok := byName["worker"]; !ok {
		t.Errorf("worker process missing; seeds = %v", names(seeds))
	}
}

func TestDetectFly_AbsentFile(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{"Dockerfile": &fstest.MapFile{Data: []byte("FROM scratch")}}
	seeds, _, _, err := detectFly(fsys)
	if err != nil {
		t.Fatalf("detectFly: %v", err)
	}
	if len(seeds) != 0 {
		t.Errorf("seeds = %v, want empty", names(seeds))
	}
}
