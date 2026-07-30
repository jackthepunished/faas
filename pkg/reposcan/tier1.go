package reposcan

// The seven tier-1 detectors live in dedicated files (compose.go,
// procfile.go, k8s.go, render.go, fly.go, serverless.go,
// appyaml.go). Each returns:
//
//   (seeds, managed, warnings, err)
//
// Seeds carry source = "<file>: <section>" provenance. Managed is
// only populated by compose's image:-without-build: path and by
// render.yaml's pserviced service block. Warnings is non-fatal:
// skip-with-reason strings. err is reserved for fs.ValidPath
// failures inside readValidFile; absent-file paths are quietly
// skipped (the tarball may not contain every format).
