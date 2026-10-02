package sling

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestShouldPromoteWorkflowLaunchStatus pins the statuses a sling launch
// promotes from: a bead nothing has started, however its status is spelled.
func TestShouldPromoteWorkflowLaunchStatus(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{status: "", want: true},
		{status: "open", want: true},
		{status: "ready", want: true},
		{status: "todo", want: true},
		{status: "triage", want: true},
		{status: "backlog", want: true},
		{status: "  Open\t", want: true},
		{status: "READY", want: true},
		{status: "in_progress", want: false},
		{status: "closed", want: false},
		{status: "blocked", want: false},
		{status: " CLOSED ", want: false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.status), func(t *testing.T) {
			if got := ShouldPromoteWorkflowLaunchStatus(tt.status); got != tt.want {
				t.Fatalf("ShouldPromoteWorkflowLaunchStatus(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

// TestPromoteWorkflowLaunchBead pins that sling promotes whatever bead it
// launches, expanded workflow root or not, and leaves a settled bead alone.
func TestPromoteWorkflowLaunchBead(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]string
		status   string
		want     string
	}{
		{
			name: "expanded workflow root is promoted",
			metadata: map[string]string{
				beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
				beadmeta.WorkflowExpandedMetadataKey: "true",
			},
			status: "open",
			want:   "in_progress",
		},
		{
			name:     "root-only workflow root is promoted",
			metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
			status:   "open",
			want:     "in_progress",
		},
		{
			name:   "plain ready bead is promoted",
			status: "ready",
			want:   "in_progress",
		},
		{
			name:   "blocked bead stays blocked",
			status: "blocked",
			want:   "blocked",
		},
		{
			name:   "closed bead stays closed",
			status: "closed",
			want:   "closed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			bead, err := store.Create(beads.Bead{Title: "root", Type: "task", Metadata: tt.metadata})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			switch tt.status {
			case "open":
			case "closed":
				if err := store.Close(bead.ID); err != nil {
					t.Fatalf("Close: %v", err)
				}
			default:
				if err := store.Update(bead.ID, beads.UpdateOpts{Status: &tt.status}); err != nil {
					t.Fatalf("Update: %v", err)
				}
			}

			if err := PromoteWorkflowLaunchBead(store, bead.ID); err != nil {
				t.Fatalf("PromoteWorkflowLaunchBead: %v", err)
			}

			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != tt.want {
				t.Fatalf("status = %q, want %q", got.Status, tt.want)
			}
		})
	}
}
