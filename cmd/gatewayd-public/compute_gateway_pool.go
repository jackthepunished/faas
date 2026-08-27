package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/gateway"
	"github.com/onebox-faas/faas/pkg/state"
)

const computeGatewayPoolTTL = 5 * time.Second
const computeGatewayDialTimeout = 2 * time.Second

// computeGatewayNodeLister is intentionally narrower than state.Store. The
// public edge only needs the active node inventory and must not become coupled
// to the rest of the control-plane store surface.
type computeGatewayNodeLister interface {
	ActiveComputeNodes(context.Context) ([]state.ComputeNode, error)
}

type computeGatewayEndpoint struct {
	name    string
	address string
}

// computeGatewayPool is the split-box data-plane dialer. It is refreshed from
// the control-plane registry, so adding or draining a compute node does not
// require changing the public gateway unit or restarting the edge.
//
// The pool deliberately opens a new connection for each request. The
// InternalReverseProxy transport otherwise pools an idle connection under one
// logical URL, which could keep sending traffic to a node after the registry
// removed it. The small connection setup cost is the safe default until the
// transport grows per-node connection keys.
type computeGatewayPool struct {
	store computeGatewayNodeLister
	log   *slog.Logger

	mu        sync.Mutex
	endpoints []computeGatewayEndpoint
	refreshed time.Time
	next      uint64
}

func newComputeGatewayPool(store computeGatewayNodeLister, log *slog.Logger) gateway.InternalDialer {
	if log == nil {
		log = slog.Default()
	}
	return &computeGatewayPool{store: store, log: log}
}

func (p *computeGatewayPool) DialContext(ctx context.Context, _ string) (net.Conn, error) {
	endpoints, err := p.snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: refresh compute gateway inventory: %w", gateway.ErrNoComputeCapacity, err)
	}
	if len(endpoints) == 0 {
		return nil, gateway.ErrNoComputeCapacity
	}

	start := p.nextIndex(len(endpoints))
	var lastErr error
	for offset := range endpoints {
		endpoint := endpoints[(start+offset)%len(endpoints)]
		dialCtx, cancel := context.WithTimeout(ctx, computeGatewayDialTimeout)
		conn, dialErr := (&net.Dialer{}).DialContext(dialCtx, "tcp", endpoint.address)
		cancel()
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
		p.log.Warn("compute gateway unavailable; trying next node",
			"node", endpoint.name, "target", endpoint.address, "err", dialErr)
	}
	if lastErr == nil {
		lastErr = errors.New("all compute gateway dials failed")
	}
	return nil, fmt.Errorf("%w: %w", gateway.ErrNoComputeCapacity, lastErr)
}

func (p *computeGatewayPool) nextIndex(size int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	index := int(p.next % uint64(size))
	p.next++
	return index
}

func (p *computeGatewayPool) snapshot(ctx context.Context) ([]computeGatewayEndpoint, error) {
	p.mu.Lock()
	if time.Since(p.refreshed) < computeGatewayPoolTTL {
		out := append([]computeGatewayEndpoint(nil), p.endpoints...)
		p.mu.Unlock()
		return out, nil
	}
	p.mu.Unlock()

	nodes, err := p.store.ActiveComputeNodes(ctx)
	if err != nil {
		return nil, err
	}
	endpoints := make([]computeGatewayEndpoint, 0, len(nodes))
	for _, node := range nodes {
		if !node.Active || node.GatewayTargetURL == nil {
			continue
		}
		address, ok := parseComputeGatewayTarget(*node.GatewayTargetURL)
		if !ok {
			p.log.Warn("ignoring invalid compute gateway target", "node", node.Name, "target", *node.GatewayTargetURL)
			continue
		}
		endpoints = append(endpoints, computeGatewayEndpoint{name: node.Name, address: address})
	}

	p.mu.Lock()
	p.endpoints = endpoints
	p.refreshed = time.Now()
	out := append([]computeGatewayEndpoint(nil), p.endpoints...)
	p.mu.Unlock()
	return out, nil
}

func parseComputeGatewayTarget(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "tcp" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", false
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		return "", false
	}
	return u.Host, true
}
