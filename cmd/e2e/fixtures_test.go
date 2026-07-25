//go:build metal

// fixtures_test.go — minimal customer source tarballs for the M6 §14
// orchestrator e2e (issue #57). Built at test runtime so the repo doesn't
// carry checked-in binary blobs; the contents are the smallest realistic
// examples for each framework the build VM supports (ADR-004):
//
//   NodeFixture      — package.json + index.js, no native deps. Railpack
//                      auto-detects from package.json.
//   PythonFixture    — requirements.txt + app.py with Flask. Railpack
//                      auto-detects from requirements.txt.
//   DockerfileFixture — single-stage Dockerfile FROM busybox (no egress
//                      needed). buildctl --frontend dockerfile.
//
// Why runtime-built, not //go:embed: the tarballs are tiny (a few hundred
// bytes each) but committing them creates a maintenance trap — any change
// to apid's tarball validator (cmd/apid/deploy_inputs.go::validateTarballShape)
// would silently invalidate the checked-in blob, and the test would only
// fail on the next CI run. Constructing from archive/tar at test time makes
// the fixture self-documenting and the round-trip explicit.
//
// Build tag: metal. The fixtures themselves don't need /dev/kvm — they're
// pure Go archive/tar — but they live next to the test that consumes them
// (cmd/e2e/build_metal_test.go) which IS metal-gated.

package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
	"time"
)

// NodeFixture returns the bytes of a minimal Node 22 source tarball.
// Railpack auto-detects Node from package.json (spec §4.5); the contents
// are the smallest app that survives `railpack build` without native deps.
func NodeFixture(t *testing.T) []byte {
	t.Helper()
	const pkgJSON = `{
  "name": "faas-fixture-node",
  "version": "1.0.0",
  "private": true,
  "engines": {"node": "22"},
  "scripts": {"start": "node index.js"},
  "dependencies": {}
}
`
	const indexJS = `const http = require('http');
http.createServer((req, res) => {
  res.writeHead(200, {'content-type': 'text/plain'});
  res.end('hello from faas (node fixture)\n');
}).listen(3000, () => console.log('node fixture listening on :3000'));
`
	files := map[string]string{
		"package.json":     pkgJSON,
		"index.js":         indexJS,
		".faas-fixture":    "node22\n",
		"faas-build-token": time.Now().UTC().Format(time.RFC3339Nano) + "\n",
	}
	return buildTarGz(t, files)
}

// PythonFixture returns the bytes of a minimal Python 3.12 source tarball
// with Flask as the single dep. Railpack auto-detects from requirements.txt
// and uses uvicorn+gunicorn under the hood; we don't pin that here because
// it's the runner's choice — the fixture only has to give Railpack a
// recognizable project shape.
func PythonFixture(t *testing.T) []byte {
	t.Helper()
	const reqs = "flask==3.0.3\n"
	const appPy = `from flask import Flask
app = Flask(__name__)

@app.route("/")
def hello():
    return "hello from faas (python fixture)\n"

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=3000)
`
	files := map[string]string{
		"requirements.txt": reqs,
		"app.py":           appPy,
		".faas-fixture":    "python312\n",
		"faas-build-token": time.Now().UTC().Format(time.RFC3339Nano) + "\n",
	}
	return buildTarGz(t, files)
}

// DockerfileFixture returns the bytes of a tarball whose root contains a
// Dockerfile. We use `FROM busybox` because the Lima builder VM has
// busybox in its base rootfs (faas-builder-bas), and busybox-static is
// also installed via the apt-get line in deploy/lima/faas-metal.yaml —
// no egress required, no Docker Hub dependency, exercises the buildctl
// --frontend dockerfile path end-to-end without a real registry.
//
// The CMD uses busybox httpd so waitReady can probe :3000 the same way it
// does for the node/python fixtures.
func DockerfileFixture(t *testing.T) []byte {
	t.Helper()
	// Plain string concat — using a raw string with interpolated time.Now
	// trips go vet's const-evaluation heuristic on the (test fixture) parens.
	dockerfile := "FROM busybox:1.37\n" +
		"LABEL org.opencontainers.image.title=\"faas-fixture-dockerfile\"\n" +
		"LABEL org.opencontainers.image.source=\"https://github.com/onebox-faas/faas test fixture\"\n" +
		"LABEL faas.build.token=\"" + time.Now().UTC().Format(time.RFC3339Nano) + "\"\n" +
		"RUN adduser -D -u 1000 app\n" +
		"USER app\n" +
		"EXPOSE 3000\n" +
		"CMD [\"/bin/busybox\", \"httpd\", \"-f\", \"-p\", \"3000\", \"-h\", \"/public\"]\n"
	files := map[string]string{
		"Dockerfile":       dockerfile,
		".faas-fixture":    "dockerfile\n",
		"faas-build-token": time.Now().UTC().Format(time.RFC3339Nano) + "\n",
	}
	return buildTarGz(t, files)
}

// GoFixture returns the bytes of a minimal Go 1.24 source tarball for the
// Railpack `--plan go` build path. The Railpack go plan emits a static
// binary at /app/server (per upstream catalog); imaged.handleDeployment
// stamps the layer manifest from the OCI image's `Cmd` via
// manifestFromImageConfig, so the static binary becomes the app's
// entrypoint. The fixture's main.go listens on :3000 to match the
// existing waitReady probe shape (and the node/python fixtures above).
//
// The fixture is the first "static binary" deploy on the app path —
// previously every runtime needed a runtime-scaffold runner shim. The
// no-shim path is the load-bearing novelty here.
func GoFixture(t *testing.T) []byte {
	t.Helper()
	const goMod = "module example.com/faas-go-fixture\n\n" +
		"go 1.24\n"
	const mainGo = `package main

import (
	"fmt"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from faas (go fixture)\n")
	})
	if err := http.ListenAndServe(":3000", nil); err != nil {
		panic(err)
	}
}
`
	files := map[string]string{
		"go.mod":           goMod,
		"main.go":          mainGo,
		".faas-fixture":    "go124\n",
		"faas-build-token": time.Now().UTC().Format(time.RFC3339Nano) + "\n",
	}
	return buildTarGz(t, files)
}

// GoDockerfileFixture returns the bytes of a tarball whose root contains
// a multi-stage Dockerfile (FROM golang AS build → FROM scratch) and a
// go.mod + main.go. Exercises the buildctl --frontend dockerfile path
// for go124 (railpack is bypassed entirely; the customer writes the
// Dockerfile). Used by the build_metal_test `go124-dockerfile-tarball`
// subtest to pin the buildctl path for the new runtime.
func GoDockerfileFixture(t *testing.T) []byte {
	t.Helper()
	return goDockerfileFixture(t, "1.24-bookworm", "go124-dockerfile")
}

// GoAlpineDockerfileFixture mirrors GoDockerfileFixture but uses
// `FROM golang:1.24-alpine AS build` so the customer's compiled binary
// is a fully-static musl-linked executable. Exercises the buildctl
// path for runtime=go124-alpine (Tier 2 PR) — the produced /app/server
// must load on a musl base (`/srv/fc/base/runner-go124-alpine.ext4`)
// and would fail with `exec format error` against the bookworm base.
// Used by the build_metal_test `go124-alpine-tarball` subtest.
func GoAlpineDockerfileFixture(t *testing.T) []byte {
	t.Helper()
	return goDockerfileFixture(t, "1.24-alpine", "go124-alpine-dockerfile")
}

// goDockerfileFixture is the shared fixture body for the bookworm +
// alpine go124 paths. The bookworm + alpine subtests differ only in the
// build-stage FROM line and the .faas-fixture label (used by buildctl's
// per-fixture cache key + the metal subtest's assertion).
func goDockerfileFixture(t *testing.T, golangTag, label string) []byte {
	t.Helper()
	const goMod = "module example.com/faas-go-dockerfile-fixture\n\n" +
		"go 1.24\n"
	const mainGo = `package main

import (
	"fmt"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from faas (go dockerfile fixture)\n")
	})
	if err := http.ListenAndServe(":3000", nil); err != nil {
		panic(err)
	}
}
`
	dockerfile := "FROM golang:" + golangTag + " AS build\n" +
		"WORKDIR /src\n" +
		"COPY go.mod main.go ./\n" +
		"RUN CGO_ENABLED=0 go build -o /out/server ./\n" +
		"FROM scratch\n" +
		"COPY --from=build /out/server /app/server\n" +
		"LABEL org.opencontainers.image.title=\"faas-fixture-go-dockerfile\"\n" +
		"LABEL faas.build.token=\"" + time.Now().UTC().Format(time.RFC3339Nano) + "\"\n" +
		"EXPOSE 3000\n" +
		"CMD [\"/app/server\"]\n"
	files := map[string]string{
		"Dockerfile":       dockerfile,
		"go.mod":           goMod,
		"main.go":          mainGo,
		".faas-fixture":    label + "\n",
		"faas-build-token": time.Now().UTC().Format(time.RFC3339Nano) + "\n",
	}
	return buildTarGz(t, files)
}

// buildTarGz packs a flat name→content map into a gzipped tar. Files are
// stored with mode 0644 and a fixed mtime so the tar headers are stable
// across runs. The body bytes are *not* byte-stable: every fixture embeds
// a `faas-build-token` whose value is `time.Now()` at construction time,
// so two fixtures called within the same tick still get distinct content
// hashes. apid's validateTarballShape doesn't care; the timestamps exist
// to make duplicate-build dedup impossible (a CI cache that mistook two
// calls' tarballs for one would mask a fixture regression).
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("buildTarGz: WriteHeader(%s): %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("buildTarGz: Write(%s): %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("buildTarGz: tar.Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("buildTarGz: gzip.Close: %v", err)
	}
	return buf.Bytes()
}
