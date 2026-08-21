package main

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/roleTemplating"
)

func TestCanonicalComputeNodeName(t *testing.T) {
	tests := []struct {
		name string
		role roleTemplating.Role
		want string
	}{
		{name: "fsn-2", role: roleTemplating.RoleComputeOnly, want: "fsn-2.faas"},
		{name: "fsn-2.faas", role: roleTemplating.RoleComputeOnly, want: "fsn-2.faas"},
		{name: "faas-control-plane", role: roleTemplating.RoleControlPlane, want: "faas-control-plane"},
		{name: "default-local", role: "", want: "default-local"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalComputeNodeName(tt.name, tt.role); got != tt.want {
				t.Fatalf("canonicalComputeNodeName(%q, %q) = %q, want %q", tt.name, tt.role, got, tt.want)
			}
		})
	}
}
