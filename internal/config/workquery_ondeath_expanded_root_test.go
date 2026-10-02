package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The default on_death hook releases the in_progress work a dying agent holds:
// it clears the assignee and reopens the bead so the routed queue can serve it
// again. An expanded workflow root (beadmeta.IsExpandedWorkflowRoot) the agent
// held is a container whose child steps are the work — nothing claims it once
// it is open — so the hook clears its assignee and leaves it in_progress, the
// state a launch leaves such a root in. Every other shape is still reopened.
//
// These tests EXECUTE the generated hook against a fake `bd` and read the
// releases out of its invocation log. The rule lives in the hook's jq, which no
// `bd` flag expresses, so a test that inspects the hook's flags cannot see it.

// onDeathAgent is the dying pool seat the hook under test belongs to.
const onDeathAgent = "worker-1"

// The writes the hook issues, as the fake `bd` logs the arguments that follow
// `update <id>`. A cleared assignee is an empty argument, so it logs as nothing.
const (
	onDeathAssigneeCleared = "--assignee "
	onDeathReopened        = "--assignee  --status open"
)

// onDeathReopenedWithRoute is the reopen of a row that carried no route: the
// hook backfills gc.run_target so the reopened row stays reachable.
func onDeathReopenedWithRoute(route string) string {
	return onDeathReopened + " --set-metadata " + beadmeta.RunTargetMetadataKey + "=" + route
}

// onDeathCorpus is expandedRootCorpus as the hook's reads return it: the same
// four routed shapes, in_progress and held by the dying agent.
func onDeathCorpus() []expandedRootRow {
	rows := expandedRootCorpus(expandedRootRoute)
	for i := range rows {
		rows[i].Status = "in_progress"
		rows[i].Assignee = onDeathAgent
	}
	return rows
}

// onDeathUnroutedCorpus is two rows the dying agent holds that carry neither
// gc.routed_to nor gc.run_target: an expanded workflow root and an ordinary
// bead.
func onDeathUnroutedCorpus() []expandedRootRow {
	return []expandedRootRow{
		{
			ID:       "expanded-root",
			Status:   "in_progress",
			Assignee: onDeathAgent,
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
				beadmeta.WorkflowExpandedMetadataKey: "true",
			},
		},
		{
			ID:       "unrouted-step",
			Status:   "in_progress",
			Assignee: onDeathAgent,
			Metadata: map[string]string{},
		},
	}
}

// onDeathIDLessRowsJSON is rows the dying agent holds that name no bead — an
// empty id, a null id and no id key, one per shape the hook tells apart —
// followed by one ordinary routed step.
func onDeathIDLessRowsJSON() string {
	held := `"status":"in_progress","assignee":"` + onDeathAgent + `"`
	return `[` +
		`{"id":"",` + held + `,"metadata":{"` + beadmeta.RoutedToMetadataKey + `":"` + expandedRootRoute + `"}},` +
		`{"id":null,` + held + `,"metadata":{}},` +
		`{` + held + `,"metadata":{"` + beadmeta.KindMetadataKey + `":"` + beadmeta.KindWorkflow + `","` + beadmeta.WorkflowExpandedMetadataKey + `":"true"}},` +
		`{"id":"ready-step",` + held + `,"metadata":{"` + beadmeta.RoutedToMetadataKey + `":"` + expandedRootRoute + `"}}` +
		`]`
}

// onDeathReads is what a fake `bd` answers for each of the hook's reads.
type onDeathReads struct {
	assignedList string
	ephemeral    string
}

// fakeOnDeathBD answers the hook's assignee list and its in_progress ephemeral
// scan independently, and records each invocation in $BD_LOG, where a release
// shows up as an `update` line. An update whose argv matches failUpdateGlob
// fails with "bd: boom" on stderr; an empty glob fails none.
func fakeOnDeathBD(reads onDeathReads, failUpdateGlob string) string {
	failArm := ""
	if failUpdateGlob != "" {
		failArm = "      " + failUpdateGlob + ")\n        echo 'bd: boom' >&2\n        exit 1\n        ;;\n"
	}
	return `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$BD_LOG"
case "$1" in
  list)
    case "$*" in
      *"--assignee=` + onDeathAgent + ` --status=in_progress"*)
        printf '%s' '` + reads.assignedList + `'
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
    case "$*" in
` + failArm + `      *)
        ;;
    esac
    ;;
  *)
    exit 1
    ;;
esac
`
}

// runOnDeathHook runs the default on_death hook of the dying pool seat on
// expandedRootRoute against a fake `bd`. It returns the write the hook issued
// for each id — the arguments after `update <id>` — and the hook's stdout,
// where a failed release is reported.
func runOnDeathHook(t *testing.T, reads onDeathReads, failUpdateGlob string) (writes map[string]string, stdout string) {
	t.Helper()
	requireJQ(t)
	a := Agent{Name: onDeathAgent, PoolName: expandedRootRoute}
	logPath := filepath.Join(t.TempDir(), "bd.log")
	stdout = runShellWithFakeBd(t, a.EffectiveOnDeath(), map[string]string{"BD_LOG": logPath}, fakeOnDeathBD(reads, failUpdateGlob))

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hook log: %v", err)
	}
	writes = make(map[string]string)
	for _, line := range strings.Split(string(log), "\n") {
		update, found := strings.CutPrefix(line, "update ")
		if !found {
			continue
		}
		id, args, found := strings.Cut(update, " ")
		if !found {
			t.Fatalf("on_death issued an update with no arguments: %q", line)
		}
		if previous, seen := writes[id]; seen {
			t.Fatalf("on_death wrote %s twice: %q then %q", id, previous, args)
		}
		writes[id] = args
	}
	return writes, stdout
}

// onDeathArm is one read of the hook, answered with rows while the other read
// is empty.
type onDeathArm struct {
	name  string
	reads func(rows string) onDeathReads
}

func onDeathArms() []onDeathArm {
	return []onDeathArm{
		{
			name:  "assignee list",
			reads: func(rows string) onDeathReads { return onDeathReads{assignedList: rows, ephemeral: "[]"} },
		},
		{
			name:  "ephemeral scan",
			reads: func(rows string) onDeathReads { return onDeathReads{assignedList: "[]", ephemeral: rows} },
		},
	}
}

// TestOnDeathKeepsExpandedWorkflowRootInProgress pins the rule: for an
// in_progress expanded workflow root the dying agent held, the hook clears the
// assignee and writes no status, whichever read returns the root.
func TestOnDeathKeepsExpandedWorkflowRootInProgress(t *testing.T) {
	for _, arm := range onDeathArms() {
		t.Run(arm.name, func(t *testing.T) {
			rows := expandedRootRowsJSON(t, onDeathCorpus()...)
			writes, _ := runOnDeathHook(t, arm.reads(rows), "")
			if got := writes["expanded-root"]; got != onDeathAssigneeCleared {
				t.Fatalf("on_death wrote %q to the expanded workflow root, want %q: a container is released by clearing its assignee and stays in_progress", got, onDeathAssigneeCleared)
			}
		})
	}
}

// TestOnDeathStillReopensRootOnlyAndAttemptRoots pins what the rule must not
// reach: a root-only workflow root (kind workflow, no marker), a marked attempt
// root (gc.kind=task) and an ordinary step are all still reopened with the
// assignee cleared. A rule keyed on kind alone strands the first in_progress;
// one keyed on the marker alone strands the second.
func TestOnDeathStillReopensRootOnlyAndAttemptRoots(t *testing.T) {
	for _, arm := range onDeathArms() {
		t.Run(arm.name, func(t *testing.T) {
			rows := expandedRootRowsJSON(t, onDeathCorpus()...)
			writes, _ := runOnDeathHook(t, arm.reads(rows), "")
			for _, id := range []string{"root-only", "attempt-root", "ready-step"} {
				if got := writes[id]; got != onDeathReopened {
					t.Errorf("on_death wrote %q to %s, want %q", got, id, onDeathReopened)
				}
			}
		})
	}
}

// TestOnDeathReadsABooleanExpandedMarker pins the marker's encoding out of the
// rule: a store may hold gc.workflow_expanded as the JSON boolean true rather
// than the string "true", and the hook must read the two alike. A workflow root
// marked true stays in_progress; every shape the string corpus reopens, and a
// workflow root marked false, are still reopened.
func TestOnDeathReadsABooleanExpandedMarker(t *testing.T) {
	for _, arm := range onDeathArms() {
		t.Run(arm.name, func(t *testing.T) {
			rows := booleanMarkerRowsJSON(t, onDeathCorpus())
			writes, _ := runOnDeathHook(t, arm.reads(rows), "")
			if got := writes["expanded-root"]; got != onDeathAssigneeCleared {
				t.Errorf("on_death wrote %q to a workflow root marked boolean true, want %q", got, onDeathAssigneeCleared)
			}
			for _, id := range []string{"root-only", "attempt-root", "ready-step", "false-marker-root"} {
				if got := writes[id]; got != onDeathReopened {
					t.Errorf("on_death wrote %q to %s, want %q", got, id, onDeathReopened)
				}
			}
		})
	}
}

// TestOnDeathBackfillsRouteOnlyOnReopenedRows pins the gc.run_target backfill to
// the rows the hook reopens. An unrouted ordinary bead is reopened with the
// dying agent's route so the routed queue can reach it. An unrouted expanded
// workflow root is not reopened, so it gets no route: its assignee is cleared
// and nothing else is written.
func TestOnDeathBackfillsRouteOnlyOnReopenedRows(t *testing.T) {
	for _, arm := range onDeathArms() {
		t.Run(arm.name, func(t *testing.T) {
			rows := expandedRootRowsJSON(t, onDeathUnroutedCorpus()...)
			writes, _ := runOnDeathHook(t, arm.reads(rows), "")
			if got, want := writes["unrouted-step"], onDeathReopenedWithRoute(expandedRootRoute); got != want {
				t.Errorf("on_death wrote %q to an unrouted bead, want %q", got, want)
			}
			if got := writes["expanded-root"]; got != onDeathAssigneeCleared {
				t.Errorf("on_death wrote %q to an unrouted expanded workflow root, want %q", got, onDeathAssigneeCleared)
			}
		})
	}
}

// TestOnDeathIssuesNoWriteForARowWithoutAnID pins the hook to rows that name a
// bead: a row whose id is empty, null or absent produces no `bd update` at all,
// whichever read returns it, and the rows around it are still released.
func TestOnDeathIssuesNoWriteForARowWithoutAnID(t *testing.T) {
	for _, arm := range onDeathArms() {
		t.Run(arm.name, func(t *testing.T) {
			writes, _ := runOnDeathHook(t, arm.reads(onDeathIDLessRowsJSON()), "")
			if got := writes["ready-step"]; got != onDeathReopened {
				t.Errorf("on_death wrote %q to ready-step, want %q", got, onDeathReopened)
			}
			delete(writes, "ready-step")
			if len(writes) != 0 {
				t.Errorf("on_death issued writes for rows without an id: %v", writes)
			}
		})
	}
}

// TestOnDeathReportsFailedExpandedWorkflowRootRelease pins the recovery
// diagnostic on the expanded-root write: when the assignee-only update fails,
// the hook still exits 0 and prints the marked line carrying bd's error, which
// is the only signal the controller surfaces.
func TestOnDeathReportsFailedExpandedWorkflowRootRelease(t *testing.T) {
	rows := expandedRootRowsJSON(t, onDeathCorpus()...)
	// Every reopen carries --status, so this glob fails the assignee-only write
	// and no other.
	failAssigneeOnlyUpdate := `"update expanded-root --assignee "`
	_, stdout := runOnDeathHook(t, onDeathReads{assignedList: rows, ephemeral: "[]"}, failAssigneeOnlyUpdate)

	want := RecoveryHookMarker + " on_death release failed for expanded-root: bd: boom"
	if got := strings.TrimSpace(stdout); got != want {
		t.Fatalf("on_death stdout = %q, want %q", got, want)
	}
}
