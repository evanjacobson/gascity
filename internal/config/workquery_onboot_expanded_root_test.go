package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The default on_boot hook reopens ownerless in_progress work routed to a
// template so a controller restart cannot strand it. An expanded workflow root
// (beadmeta.IsExpandedWorkflowRoot) is in_progress and ownerless by design — it
// is a container whose child steps are the work — so the hook must leave it
// alone, and must still reopen every other routed shape.
//
// These tests EXECUTE the generated hook against a fake `bd` and read the
// reopens out of its invocation log. The rule lives in the hook's jq, which no
// `bd` flag expresses, so a test that inspects the hook's flags cannot see it.

// onBootCorpus is expandedRootCorpus as the hook's reads return it: the same
// four routed shapes, in_progress and unassigned.
func onBootCorpus(route string) []expandedRootRow {
	rows := expandedRootCorpus(route)
	for i := range rows {
		rows[i].Status = "in_progress"
	}
	return rows
}

// onBootRunTargetCorpus is the two shapes the legacy gc.run_target list can
// return — it asks `bd` for gc.kind=workflow rows only — routed by gc.run_target
// alone, in_progress and unassigned: an expanded workflow root and a root-only
// one.
func onBootRunTargetCorpus(route string) []expandedRootRow {
	runTargetRoot := func(id string, meta map[string]string) expandedRootRow {
		metadata := map[string]string{
			beadmeta.KindMetadataKey:      beadmeta.KindWorkflow,
			beadmeta.RunTargetMetadataKey: route,
		}
		for k, v := range meta {
			metadata[k] = v
		}
		return expandedRootRow{ID: id, Status: "in_progress", Metadata: metadata}
	}
	return []expandedRootRow{
		runTargetRoot("expanded-root", map[string]string{beadmeta.WorkflowExpandedMetadataKey: "true"}),
		runTargetRoot("root-only", nil),
	}
}

// onBootReads is what a fake `bd` answers for each of the hook's reads.
type onBootReads struct {
	routedList    string
	runTargetList string
	ephemeral     string
}

// fakeOnBootBD answers the hook's gc.routed_to list, its legacy gc.run_target
// list and its in_progress ephemeral scan independently, and records each
// invocation in $BD_LOG, where a reopen shows up as an `update` line.
func fakeOnBootBD(route string, reads onBootReads) string {
	return `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$BD_LOG"
case "$1" in
  list)
    case "$*" in
      ` + routedReadGlob(route) + `)
        printf '%s' '` + reads.routedList + `'
        ;;
      ` + runTargetReadGlob(route) + `)
        printf '%s' '` + reads.runTargetList + `'
        ;;
      *)
        printf '[]'
        ;;
    esac
    ;;
  query)
    case "$*" in
      *"ephemeral=true AND status=in_progress"*)
        printf '%s' '` + reads.ephemeral + `'
        ;;
      *)
        printf '[]'
        ;;
    esac
    ;;
  update)
    ;;
  *)
    exit 1
    ;;
esac
`
}

// onBootReopenedIDs runs the default on_boot hook of a pool seat on route
// against reads and returns the ids it reopened, in order.
func onBootReopenedIDs(t *testing.T, route string, reads onBootReads) []string {
	t.Helper()
	requireJQ(t)
	a := Agent{Name: "worker-1", PoolName: route}
	log := runLifecycleHookCommand(t, a.EffectiveOnBoot(), fakeOnBootBD(route, reads))

	var ids []string
	for _, line := range strings.Split(log, "\n") {
		id, found := strings.CutPrefix(line, "update ")
		if !found {
			continue
		}
		id, found = strings.CutSuffix(id, " --status open")
		if !found {
			t.Fatalf("on_boot issued a write that is not a reopen: %q", line)
		}
		ids = append(ids, id)
	}
	return ids
}

// onBootArm is one read of the hook, answered with rows while the other reads
// are empty. Every arm's rows hold the expanded workflow root as "expanded-root";
// stillReopened names the rest, which the rule must not reach.
type onBootArm struct {
	name          string
	rows          []expandedRootRow
	reads         func(rows string) onBootReads
	stillReopened []string
}

func onBootArms() []onBootArm {
	return []onBootArm{
		{
			name: "routed_to list",
			rows: onBootCorpus(expandedRootRoute),
			reads: func(rows string) onBootReads {
				return onBootReads{routedList: rows, runTargetList: "[]", ephemeral: "[]"}
			},
			stillReopened: []string{"root-only", "attempt-root", "ready-step"},
		},
		{
			name: "run_target list",
			rows: onBootRunTargetCorpus(expandedRootRoute),
			reads: func(rows string) onBootReads {
				return onBootReads{routedList: "[]", runTargetList: rows, ephemeral: "[]"}
			},
			stillReopened: []string{"root-only"},
		},
		{
			name: "ephemeral scan",
			rows: onBootCorpus(expandedRootRoute),
			reads: func(rows string) onBootReads {
				return onBootReads{routedList: "[]", runTargetList: "[]", ephemeral: rows}
			},
			stillReopened: []string{"root-only", "attempt-root", "ready-step"},
		},
	}
}

func (arm onBootArm) reopenedIDs(t *testing.T) []string {
	t.Helper()
	return onBootReopenedIDs(t, expandedRootRoute, arm.reads(expandedRootRowsJSON(t, arm.rows...)))
}

// TestOnBootDoesNotReopenExpandedWorkflowRoot pins the rule: the hook issues no
// reopen for an in_progress expanded workflow root, whichever read returns it.
func TestOnBootDoesNotReopenExpandedWorkflowRoot(t *testing.T) {
	for _, arm := range onBootArms() {
		t.Run(arm.name, func(t *testing.T) {
			got := arm.reopenedIDs(t)
			if len(got) == 0 {
				t.Fatal("on_boot reopened nothing; the read under test was never exercised")
			}
			if slices.Contains(got, "expanded-root") {
				t.Fatalf("on_boot reopened %v; an expanded workflow root is a container and must stay in_progress", got)
			}
		})
	}
}

// TestOnBootStillReopensRootOnlyAndAttemptRoots pins what the rule must not
// reach: a root-only workflow root (kind workflow, no marker), a marked attempt
// root (gc.kind=task) and an ordinary step are all still reopened. A skip keyed
// on kind alone strands the first; one keyed on the marker alone strands the
// second. The legacy gc.run_target list returns workflow roots only, so there
// the root-only root is the whole of it.
func TestOnBootStillReopensRootOnlyAndAttemptRoots(t *testing.T) {
	for _, arm := range onBootArms() {
		t.Run(arm.name, func(t *testing.T) {
			got := arm.reopenedIDs(t)
			for _, want := range arm.stillReopened {
				if !slices.Contains(got, want) {
					t.Errorf("on_boot reopened %v, want %s among them", got, want)
				}
			}
		})
	}
}

// TestOnBootReadsABooleanExpandedMarker pins the marker's encoding out of the
// rule: a store may hold gc.workflow_expanded as the JSON boolean true rather
// than the string "true", and the hook must read the two alike. A workflow root
// marked true is not reopened; every shape the string corpus keeps, and a
// workflow root marked false, still are.
func TestOnBootReadsABooleanExpandedMarker(t *testing.T) {
	for _, arm := range onBootArms() {
		t.Run(arm.name, func(t *testing.T) {
			got := onBootReopenedIDs(t, expandedRootRoute, arm.reads(booleanMarkerRowsJSON(t, arm.rows)))
			if slices.Contains(got, "expanded-root") {
				t.Errorf("on_boot reopened %v; a boolean-true marker on a workflow root is an expanded root and must stay in_progress", got)
			}
			for _, want := range append(slices.Clone(arm.stillReopened), "false-marker-root") {
				if !slices.Contains(got, want) {
					t.Errorf("on_boot reopened %v, want %s among them", got, want)
				}
			}
		})
	}
}
