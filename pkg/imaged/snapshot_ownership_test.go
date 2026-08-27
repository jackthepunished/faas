package imaged

import "testing"

func TestHandlesSnapshotBoot(t *testing.T) {
	tests := []struct {
		name  string
		local string
		owner string
		want  bool
	}{
		{name: "legacy single box", local: "", owner: "", want: true},
		{name: "named daemon accepts legacy event", local: "fsn-2.faas", owner: "", want: true},
		{name: "legacy daemon accepts named event", local: "", owner: "fsn-2.faas", want: true},
		{name: "matching owner", local: "fsn-2.faas", owner: "fsn-2.faas", want: true},
		{name: "sibling owner", local: "fsn-2.faas", owner: "fsn-3.faas", want: false},
		{name: "trims identities", local: " fsn-2.faas ", owner: "fsn-2.faas", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := handlesSnapshotBoot(tt.local, tt.owner); got != tt.want {
				t.Fatalf("handlesSnapshotBoot(%q, %q) = %v, want %v", tt.local, tt.owner, got, tt.want)
			}
		})
	}
}
