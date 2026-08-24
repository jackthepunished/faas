// Package main — dial.go wraps the production vmmd dial so
// cmd/builderd/readiness.go's defaultDial seam doesn't pull
// google.golang.org/grpc into every test (the readiness tests
// inject a stub via BuildReadinessProbe's dial param).
package main

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// dialGRPC performs a non-blocking dial against target. We
// don't keep the connection open — the production driver
// (cmd/builderd/main.go) holds its own client. /readyz only
// needs to know "can we reach vmmd".
func dialGRPC(ctx context.Context, target string) error {
	conn, err := grpc.DialContext(
		ctx,
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return err
	}
	// /readyz only needs the dial to succeed; close immediately.
	_ = conn.Close()
	return nil
}
