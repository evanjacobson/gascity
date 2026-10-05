package sling

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// requireDefaultFormulaAttaches slings a source bead carrying metadata to a
// target with a default_sling_formula and requires the sling to succeed with
// a wisp attached to the source.
func requireDefaultFormulaAttaches(t *testing.T, metadata map[string]string) {
	t.Helper()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("code-review")}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{Title: "source", Type: "task", Metadata: metadata})
	if err != nil {
		t.Fatalf("store.Create(source): %v", err)
	}

	result, err := DoSling(testOpts(a, source.ID), deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.WispRootID == "" {
		t.Fatal("WispRootID is empty, want a wisp attached to the source bead")
	}
	got, err := deps.Store.Get(source.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", source.ID, err)
	}
	if got.Metadata[beadmeta.MoleculeIDMetadataKey] != result.WispRootID {
		t.Fatalf("source molecule_id = %q, want %q", got.Metadata[beadmeta.MoleculeIDMetadataKey], result.WispRootID)
	}
}

// TestDoSlingAcceptsRootOnlyWorkflowRootAsSourceBead pins that a workflow root
// whose steps were never materialized stays slingable: it carries the workflow
// kind without the expanded marker and is itself the unit of work.
func TestDoSlingAcceptsRootOnlyWorkflowRootAsSourceBead(t *testing.T) {
	requireDefaultFormulaAttaches(t, map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
	})
}

// TestDoSlingAcceptsMarkedAttemptRootAsSourceBead pins that the expanded
// marker alone does not make a bead unslingable: a retry or ralph attempt root
// carries the marker with gc.kind=task and is real work.
func TestDoSlingAcceptsMarkedAttemptRootAsSourceBead(t *testing.T) {
	requireDefaultFormulaAttaches(t, map[string]string{
		beadmeta.KindMetadataKey:             beadmeta.KindTask,
		beadmeta.WorkflowExpandedMetadataKey: "true",
	})
}

// TestDoSlingPlainRoutesCookedExpandedWorkflowRoot pins that the refusal is
// about wrapping a formula around the root, not about the root itself. A
// cooked expanded workflow root slung to a target with no default formula is
// a plain bead route, and it goes through as before: routed to the target,
// with nothing attached.
func TestDoSlingPlainRoutesCookedExpandedWorkflowRoot(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(2)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	root, err := deps.Store.Create(beads.Bead{
		Title:  "workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
			beadmeta.WorkflowExpandedMetadataKey: "true",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(root): %v", err)
	}

	result, err := DoSling(testOpts(a, root.ID), deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v, want a plain route of the cooked root", err)
	}
	if result.Method != "bead" {
		t.Fatalf("Method = %q, want bead", result.Method)
	}
	if result.WispRootID != "" {
		t.Fatalf("WispRootID = %q, want no formula attached on a plain route", result.WispRootID)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %#v, want the one route call", runner.calls)
	}
}

// TestDoSlingRefusesFormulaBackedRouteOfExpandedWorkflowRoot pins the typed
// refusal on both formula-backed routes, the target's default_sling_formula
// and an explicit --on, each plain, with --force and under dry-run. Every case
// returns an *ExpandedWorkflowRootError naming the root and leaves the store,
// the runner and the router untouched.
func TestDoSlingRefusesFormulaBackedRouteOfExpandedWorkflowRoot(t *testing.T) {
	routes := []struct {
		name      string
		target    config.Agent
		onFormula string
	}{
		{
			name:   "default formula",
			target: config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("code-review")},
		},
		{
			name:      "explicit on",
			target:    config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)},
			onFormula: "code-review",
		},
	}
	modes := []struct {
		name   string
		force  bool
		dryRun bool
	}{
		{name: "plain"},
		{name: "force", force: true},
		{name: "dry run", dryRun: true},
	}
	for _, route := range routes {
		for _, mode := range modes {
			t.Run(route.name+"/"+mode.name, func(t *testing.T) {
				runner := newFakeRunner()
				router := &fakeBeadRouter{}
				cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
				deps := testDeps(cfg, runtime.NewFake(), runner.run)
				deps.Router = router
				root, err := deps.Store.Create(beads.Bead{
					Title:  "workflow",
					Type:   "task",
					Status: "open",
					Metadata: map[string]string{
						beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
						beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
						beadmeta.WorkflowExpandedMetadataKey: "true",
						beadmeta.RoutedToMetadataKey:         "mayor",
					},
				})
				if err != nil {
					t.Fatalf("store.Create(root): %v", err)
				}
				if _, err := deps.Store.Create(beads.Bead{
					Title:    "step",
					Type:     "task",
					Status:   "in_progress",
					Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
				}); err != nil {
					t.Fatalf("store.Create(step): %v", err)
				}
				before, err := deps.Store.Get(root.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", root.ID, err)
				}

				opts := testOpts(route.target, root.ID)
				opts.OnFormula = route.onFormula
				opts.Force = mode.force
				opts.DryRun = mode.dryRun
				_, err = DoSling(opts, deps, deps.Store)
				var refused *ExpandedWorkflowRootError
				if !errors.As(err, &refused) {
					t.Fatalf("DoSling error = %T %[1]v, want ExpandedWorkflowRootError", err)
				}
				if refused.BeadID != root.ID {
					t.Fatalf("ExpandedWorkflowRootError.BeadID = %q, want %q", refused.BeadID, root.ID)
				}

				requireOnlySeedBeads(t, deps.Store, 2)
				after, err := deps.Store.Get(root.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", root.ID, err)
				}
				if after.Status != before.Status {
					t.Fatalf("root status = %q, want %q", after.Status, before.Status)
				}
				if after.Assignee != before.Assignee {
					t.Fatalf("root assignee = %q, want %q", after.Assignee, before.Assignee)
				}
				if !reflect.DeepEqual(after.Metadata, before.Metadata) {
					t.Fatalf("root metadata = %#v, want %#v", after.Metadata, before.Metadata)
				}
				if len(runner.calls) != 0 {
					t.Fatalf("runner calls = %#v, want none", runner.calls)
				}
				if len(router.routed) != 0 {
					t.Fatalf("router calls = %#v, want none", router.routed)
				}
			})
		}
	}
}

// expandedWorkflowRootOrigin is the pool the fixture's expanded workflow root
// is routed to. No test slings to it, so the fixture's root is never already
// routed to the batch's target;
// TestDoSlingBatchRefusesExpandedWorkflowRootChildRoutedToTarget covers the
// root that is.
const expandedWorkflowRootOrigin = "origin-pool"

// expandedWorkflowRootConvoyFixture seeds store with a convoy tracking one
// open expanded workflow root, which has one step, and one plain task, in that
// order, and returns the convoy, the root and the task.
func expandedWorkflowRootConvoyFixture(t *testing.T, store beads.Store) (convoy, root, task beads.Bead) {
	t.Helper()
	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("store.Create(convoy): %v", err)
	}
	root, err = store.Create(beads.Bead{
		Title:  "workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
			beadmeta.WorkflowExpandedMetadataKey: "true",
			beadmeta.RoutedToMetadataKey:         expandedWorkflowRootOrigin,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(root): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "step",
		Type:     "task",
		Status:   "in_progress",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	}); err != nil {
		t.Fatalf("store.Create(step): %v", err)
	}
	task, err = store.Create(beads.Bead{Title: "task", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("store.Create(task): %v", err)
	}
	for _, id := range []string{root.ID, task.ID} {
		if err := convoycore.TrackItem(store, convoy.ID, id); err != nil {
			t.Fatalf("TrackItem(%s): %v", id, err)
		}
	}
	return convoy, root, task
}

// formulaBackedBatchRoutes are the two routes on which a convoy batch attaches
// a formula to each open child: the target's default_sling_formula and an
// explicit --on.
var formulaBackedBatchRoutes = []struct {
	name       string
	target     config.Agent
	onFormula  string
	wantMethod string
}{
	{
		name:       "default formula",
		target:     config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("code-review")},
		wantMethod: "batch-default-on",
	},
	{
		name:       "explicit on",
		target:     config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)},
		onFormula:  "code-review",
		wantMethod: "batch-on",
	},
}

// formulaBackedBatchModes run each formula-backed batch route without and with
// --force, which does not lift the refusal.
var formulaBackedBatchModes = []struct {
	name  string
	force bool
}{
	{name: "plain"},
	{name: "force", force: true},
}

// TestDoSlingBatchFailsExpandedWorkflowRootChild pins the refusal on the
// convoy per-child path, which does not pass through preflight. On a
// formula-backed batch the expanded root child fails with the typed
// *ExpandedWorkflowRootError and is left untouched, and its sibling still gets
// the formula attached and is routed.
func TestDoSlingBatchFailsExpandedWorkflowRootChild(t *testing.T) {
	for _, route := range formulaBackedBatchRoutes {
		for _, mode := range formulaBackedBatchModes {
			t.Run(route.name+"/"+mode.name, func(t *testing.T) {
				runner := newFakeRunner()
				cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
				deps := testDeps(cfg, runtime.NewFake(), runner.run)
				convoy, root, task := expandedWorkflowRootConvoyFixture(t, deps.Store)

				opts := testOpts(route.target, convoy.ID)
				opts.OnFormula = route.onFormula
				opts.Force = mode.force
				result, err := DoSlingBatch(opts, deps, deps.Store)
				var refused *ExpandedWorkflowRootError
				if !errors.As(err, &refused) {
					t.Fatalf("DoSlingBatch error = %T %[1]v, want ExpandedWorkflowRootError (children = %+v)", err, result.Children)
				}
				if refused.BeadID != root.ID {
					t.Fatalf("ExpandedWorkflowRootError.BeadID = %q, want %q", refused.BeadID, root.ID)
				}

				if result.Method != route.wantMethod {
					t.Fatalf("Method = %q, want %q", result.Method, route.wantMethod)
				}
				if result.Total != 2 || result.Routed != 1 || result.Failed != 1 {
					t.Fatalf("total=%d routed=%d failed=%d, want total=2 routed=1 failed=1", result.Total, result.Routed, result.Failed)
				}
				if len(result.Children) != 2 {
					t.Fatalf("children = %+v, want the root then the task", result.Children)
				}
				wantRoot := SlingChildResult{BeadID: root.ID, Failed: true, FailReason: refused.Error()}
				if result.Children[0] != wantRoot {
					t.Fatalf("root child result = %+v, want %+v", result.Children[0], wantRoot)
				}
				sibling := result.Children[1]
				if sibling.BeadID != task.ID || !sibling.Routed || sibling.Failed || sibling.WispRootID == "" || sibling.FormulaName != "code-review" {
					t.Fatalf("task child result = %+v, want %s routed with a code-review wisp", sibling, task.ID)
				}

				after, err := deps.Store.Get(root.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", root.ID, err)
				}
				if !reflect.DeepEqual(after, root) {
					t.Fatalf("root = %+v, want unchanged %+v", after, root)
				}
				gotTask, err := deps.Store.Get(task.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", task.ID, err)
				}
				if gotTask.Metadata[beadmeta.MoleculeIDMetadataKey] != sibling.WispRootID {
					t.Fatalf("task molecule_id = %q, want %q", gotTask.Metadata[beadmeta.MoleculeIDMetadataKey], sibling.WispRootID)
				}
				if len(runner.calls) != 1 || !strings.Contains(runner.calls[0], task.ID) {
					t.Fatalf("runner calls = %#v, want the one route call for %s", runner.calls, task.ID)
				}
			})
		}
	}
}

// TestDoSlingBatchRefusesExpandedWorkflowRootChildRoutedToTarget pins that the
// refusal runs ahead of the batch's already-routed skip. A retried batch whose
// expanded root child is already routed to the target fails that child with
// the typed error instead of reporting it skipped, and leaves it untouched.
func TestDoSlingBatchRefusesExpandedWorkflowRootChildRoutedToTarget(t *testing.T) {
	for _, route := range formulaBackedBatchRoutes {
		t.Run(route.name, func(t *testing.T) {
			cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
			deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
			convoy, root, _ := expandedWorkflowRootConvoyFixture(t, deps.Store)
			if err := deps.Store.SetMetadata(root.ID, beadmeta.RoutedToMetadataKey, route.target.QualifiedName()); err != nil {
				t.Fatalf("SetMetadata(%s routed_to): %v", root.ID, err)
			}
			before, err := deps.Store.Get(root.ID)
			if err != nil {
				t.Fatalf("store.Get(%s): %v", root.ID, err)
			}

			opts := testOpts(route.target, convoy.ID)
			opts.OnFormula = route.onFormula
			result, err := DoSlingBatch(opts, deps, deps.Store)
			var refused *ExpandedWorkflowRootError
			if !errors.As(err, &refused) || refused.BeadID != root.ID {
				t.Fatalf("DoSlingBatch error = %T %[1]v, want ExpandedWorkflowRootError for %s", err, root.ID)
			}
			wantRoot := SlingChildResult{BeadID: root.ID, Failed: true, FailReason: refused.Error()}
			if len(result.Children) == 0 || result.Children[0] != wantRoot {
				t.Fatalf("children = %+v, want the root failed as %+v, not skipped as already routed", result.Children, wantRoot)
			}
			if result.Routed != 1 || result.Failed != 1 || result.IdempotentCt != 0 {
				t.Fatalf("routed=%d failed=%d idempotent=%d, want routed=1 failed=1 idempotent=0", result.Routed, result.Failed, result.IdempotentCt)
			}
			after, err := deps.Store.Get(root.ID)
			if err != nil {
				t.Fatalf("store.Get(%s): %v", root.ID, err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("root = %+v, want unchanged %+v", after, before)
			}
		})
	}
}

// TestDoSlingBatchFailsWhenOnlyOpenChildIsExpandedWorkflowRoot pins the batch
// with nothing left to route: a convoy whose only open child is an expanded
// workflow root fails 1/1 with the typed error, for real and under dry-run,
// and mutates nothing.
func TestDoSlingBatchFailsWhenOnlyOpenChildIsExpandedWorkflowRoot(t *testing.T) {
	for _, route := range formulaBackedBatchRoutes {
		for _, dryRun := range []bool{false, true} {
			name := route.name
			if dryRun {
				name += "/dry run"
			}
			t.Run(name, func(t *testing.T) {
				runner := newFakeRunner()
				cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
				deps := testDeps(cfg, runtime.NewFake(), runner.run)
				convoy, root, task := expandedWorkflowRootConvoyFixture(t, deps.Store)
				if err := deps.Store.Close(task.ID); err != nil {
					t.Fatalf("store.Close(%s): %v", task.ID, err)
				}
				before, err := deps.Store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
				if err != nil {
					t.Fatalf("list beads: %v", err)
				}

				opts := testOpts(route.target, convoy.ID)
				opts.OnFormula = route.onFormula
				opts.DryRun = dryRun
				result, err := DoSlingBatch(opts, deps, deps.Store)
				var refused *ExpandedWorkflowRootError
				if !errors.As(err, &refused) || refused.BeadID != root.ID {
					t.Fatalf("DoSlingBatch error = %T %[1]v, want ExpandedWorkflowRootError for %s", err, root.ID)
				}
				if !strings.Contains(err.Error(), "1/1 children failed") {
					t.Fatalf("DoSlingBatch error = %q, want the 1/1 children failed summary", err)
				}
				if result.Total != 2 || result.Routed != 0 || result.Failed != 1 || result.Skipped != 1 {
					t.Fatalf("total=%d routed=%d failed=%d skipped=%d, want total=2 routed=0 failed=1 skipped=1", result.Total, result.Routed, result.Failed, result.Skipped)
				}
				want := []SlingChildResult{
					{BeadID: root.ID, Failed: true, FailReason: refused.Error()},
					{BeadID: task.ID, Status: "closed", Skipped: true},
				}
				if !reflect.DeepEqual(result.Children, want) {
					t.Fatalf("children = %+v, want %+v", result.Children, want)
				}
				after, err := deps.Store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
				if err != nil {
					t.Fatalf("list beads: %v", err)
				}
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("store changed:\n got %+v\nwant %+v", after, before)
				}
				if len(runner.calls) != 0 {
					t.Fatalf("runner calls = %#v, want none", runner.calls)
				}
			})
		}
	}
}

// TestDoSlingBatchSourceWorkflowConflictSubsumesExpandedWorkflowRootRefusal
// pins the batch whose refused root sits next to a sibling that already has a
// live workflow attached. The sibling's conflict fails the whole batch in the
// attachment pre-check, before any per-child result exists, so the error
// carries the typed conflict the CLI maps to exit 3 and not the refusal, and
// neither child is touched.
func TestDoSlingBatchSourceWorkflowConflictSubsumesExpandedWorkflowRootRefusal(t *testing.T) {
	for _, route := range formulaBackedBatchRoutes {
		t.Run(route.name, func(t *testing.T) {
			runner := newFakeRunner()
			cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
			deps := testDeps(cfg, runtime.NewFake(), runner.run)
			convoy, _, task := expandedWorkflowRootConvoyFixture(t, deps.Store)
			live, err := deps.Store.Create(beads.Bead{
				Title:  "live workflow",
				Type:   "task",
				Status: "in_progress",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:         beadmeta.KindWorkflow,
					beadmeta.SourceBeadIDMetadataKey: task.ID,
				},
			})
			if err != nil {
				t.Fatalf("store.Create(live workflow): %v", err)
			}
			if err := deps.Store.SetMetadata(task.ID, "workflow_id", live.ID); err != nil {
				t.Fatalf("SetMetadata(%s workflow_id): %v", task.ID, err)
			}
			before, err := deps.Store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if err != nil {
				t.Fatalf("list beads: %v", err)
			}

			opts := testOpts(route.target, convoy.ID)
			opts.OnFormula = route.onFormula
			result, err := DoSlingBatch(opts, deps, deps.Store)
			var conflict *sourceworkflow.ConflictError
			if !errors.As(err, &conflict) || conflict.SourceBeadID != task.ID {
				t.Fatalf("DoSlingBatch error = %T %[1]v, want ConflictError for %s", err, task.ID)
			}
			var refused *ExpandedWorkflowRootError
			if errors.As(err, &refused) {
				t.Fatalf("DoSlingBatch error = %v, want the conflict alone", err)
			}
			if len(result.Children) != 0 {
				t.Fatalf("children = %+v, want none before the pre-check passes", result.Children)
			}
			after, err := deps.Store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if err != nil {
				t.Fatalf("list beads: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("store changed:\n got %+v\nwant %+v", after, before)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("runner calls = %#v, want none", runner.calls)
			}
		})
	}
}

// TestDoSlingBatchLeavesAlreadyRoutedChildMoleculeAlone pins that the formula
// pre-check, which burns a live molecule on an unassigned child the batch is
// about to attach to, sees only those children. A child a retried batch skips
// as already routed keeps the molecule its first run attached, for real and
// under dry-run, where it is reported as skipped.
func TestDoSlingBatchLeavesAlreadyRoutedChildMoleculeAlone(t *testing.T) {
	for _, route := range formulaBackedBatchRoutes {
		for _, dryRun := range []bool{false, true} {
			name := route.name
			if dryRun {
				name += "/dry run"
			}
			t.Run(name, func(t *testing.T) {
				cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
				deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
				convoy, err := deps.Store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
				if err != nil {
					t.Fatalf("store.Create(convoy): %v", err)
				}
				routed, err := deps.Store.Create(beads.Bead{
					Title:    "routed",
					Type:     "task",
					Status:   "open",
					Metadata: map[string]string{beadmeta.RoutedToMetadataKey: route.target.QualifiedName()},
				})
				if err != nil {
					t.Fatalf("store.Create(routed): %v", err)
				}
				fresh, err := deps.Store.Create(beads.Bead{Title: "fresh", Type: "task", Status: "open"})
				if err != nil {
					t.Fatalf("store.Create(fresh): %v", err)
				}
				for _, id := range []string{routed.ID, fresh.ID} {
					if err := convoycore.TrackItem(deps.Store, convoy.ID, id); err != nil {
						t.Fatalf("TrackItem(%s): %v", id, err)
					}
				}
				wisp, err := deps.Store.Create(beads.Bead{Title: "attached", Type: "molecule", Status: "open"})
				if err != nil {
					t.Fatalf("store.Create(wisp): %v", err)
				}
				if err := deps.Store.SetMetadata(routed.ID, beadmeta.MoleculeIDMetadataKey, wisp.ID); err != nil {
					t.Fatalf("SetMetadata(%s molecule_id): %v", routed.ID, err)
				}
				routedBefore, err := deps.Store.Get(routed.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", routed.ID, err)
				}
				wispBefore, err := deps.Store.Get(wisp.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", wisp.ID, err)
				}

				opts := testOpts(route.target, convoy.ID)
				opts.OnFormula = route.onFormula
				opts.DryRun = dryRun
				result, err := DoSlingBatch(opts, deps, deps.Store)
				if err != nil {
					t.Fatalf("DoSlingBatch: %v", err)
				}
				if len(result.AutoBurned) != 0 {
					t.Errorf("AutoBurned = %v, want none", result.AutoBurned)
				}
				if result.Routed != 1 || result.Failed != 0 || result.Skipped != 1 || result.IdempotentCt != 1 {
					t.Errorf("routed=%d failed=%d skipped=%d idempotent=%d, want routed=1 failed=0 skipped=1 idempotent=1", result.Routed, result.Failed, result.Skipped, result.IdempotentCt)
				}
				wantSkipped := SlingChildResult{BeadID: routed.ID, Skipped: true}
				if len(result.Children) != 2 || result.Children[0] != wantSkipped {
					t.Errorf("children = %+v, want %s skipped as already routed, then %s", result.Children, routed.ID, fresh.ID)
				}

				routedAfter, err := deps.Store.Get(routed.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", routed.ID, err)
				}
				if !reflect.DeepEqual(routedAfter, routedBefore) {
					t.Errorf("already-routed child = %+v, want unchanged %+v", routedAfter, routedBefore)
				}
				wispAfter, err := deps.Store.Get(wisp.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", wisp.ID, err)
				}
				if !reflect.DeepEqual(wispAfter, wispBefore) {
					t.Errorf("attached molecule = %+v, want unchanged %+v", wispAfter, wispBefore)
				}
			})
		}
	}
}

// TestDoSlingBatchRefusesExpandedWorkflowRootChildBeforeFormulaPreCheck pins
// that a formula batch sets the refused child aside before its attachment
// pre-check, which burns a live molecule on an unassigned child it is about to
// attach a formula to. A refused root carrying such a molecule, and the
// molecule itself, come through unchanged.
func TestDoSlingBatchRefusesExpandedWorkflowRootChildBeforeFormulaPreCheck(t *testing.T) {
	for _, route := range formulaBackedBatchRoutes {
		for _, mode := range formulaBackedBatchModes {
			t.Run(route.name+"/"+mode.name, func(t *testing.T) {
				cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
				deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
				convoy, root, task := expandedWorkflowRootConvoyFixture(t, deps.Store)
				wisp, err := deps.Store.Create(beads.Bead{Title: "attached", Type: "molecule", Status: "open"})
				if err != nil {
					t.Fatalf("store.Create(wisp): %v", err)
				}
				if err := deps.Store.SetMetadata(root.ID, beadmeta.MoleculeIDMetadataKey, wisp.ID); err != nil {
					t.Fatalf("SetMetadata(%s molecule_id): %v", root.ID, err)
				}
				rootBefore, err := deps.Store.Get(root.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", root.ID, err)
				}
				wispBefore, err := deps.Store.Get(wisp.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", wisp.ID, err)
				}

				opts := testOpts(route.target, convoy.ID)
				opts.OnFormula = route.onFormula
				opts.Force = mode.force
				result, err := DoSlingBatch(opts, deps, deps.Store)
				var refused *ExpandedWorkflowRootError
				if !errors.As(err, &refused) || refused.BeadID != root.ID {
					t.Errorf("DoSlingBatch error = %T %[1]v, want ExpandedWorkflowRootError for %s", err, root.ID)
				}
				if len(result.AutoBurned) != 0 {
					t.Errorf("AutoBurned = %v, want none", result.AutoBurned)
				}
				if result.Routed != 1 || result.Failed != 1 {
					t.Errorf("routed=%d failed=%d, want routed=1 failed=1", result.Routed, result.Failed)
				}

				rootAfter, err := deps.Store.Get(root.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", root.ID, err)
				}
				if !reflect.DeepEqual(rootAfter, rootBefore) {
					t.Errorf("root = %+v, want unchanged %+v", rootAfter, rootBefore)
				}
				wispAfter, err := deps.Store.Get(wisp.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", wisp.ID, err)
				}
				if !reflect.DeepEqual(wispAfter, wispBefore) {
					t.Errorf("attached molecule = %+v, want unchanged %+v", wispAfter, wispBefore)
				}
				gotTask, err := deps.Store.Get(task.ID)
				if err != nil {
					t.Fatalf("store.Get(%s): %v", task.ID, err)
				}
				if gotTask.Metadata[beadmeta.MoleculeIDMetadataKey] == "" {
					t.Errorf("task molecule_id is empty, want the formula attached to the sibling")
				}
			})
		}
	}
}

// TestDoSlingBatchDryRunRefusesExpandedWorkflowRootChild pins that a
// formula-backed batch dry-run reports what the real run does: the expanded
// root child is refused with the typed error and counted as failed, its
// sibling as routable, and nothing is mutated.
func TestDoSlingBatchDryRunRefusesExpandedWorkflowRootChild(t *testing.T) {
	for _, route := range formulaBackedBatchRoutes {
		for _, mode := range formulaBackedBatchModes {
			t.Run(route.name+"/"+mode.name, func(t *testing.T) {
				runner := newFakeRunner()
				cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
				deps := testDeps(cfg, runtime.NewFake(), runner.run)
				convoy, root, task := expandedWorkflowRootConvoyFixture(t, deps.Store)
				before, err := deps.Store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
				if err != nil {
					t.Fatalf("list beads: %v", err)
				}

				opts := testOpts(route.target, convoy.ID)
				opts.OnFormula = route.onFormula
				opts.Force = mode.force
				opts.DryRun = true
				result, err := DoSlingBatch(opts, deps, deps.Store)
				var refused *ExpandedWorkflowRootError
				if !errors.As(err, &refused) {
					t.Fatalf("DoSlingBatch dry-run error = %T %[1]v, want ExpandedWorkflowRootError (routed=%d failed=%d)", err, result.Routed, result.Failed)
				}
				if refused.BeadID != root.ID {
					t.Fatalf("ExpandedWorkflowRootError.BeadID = %q, want %q", refused.BeadID, root.ID)
				}
				if !result.DryRun {
					t.Fatal("result.DryRun = false, want true")
				}
				if result.Total != 2 || result.Routed != 1 || result.Failed != 1 {
					t.Fatalf("total=%d routed=%d failed=%d, want total=2 routed=1 failed=1", result.Total, result.Routed, result.Failed)
				}
				want := []SlingChildResult{
					{BeadID: root.ID, Failed: true, FailReason: refused.Error()},
					{BeadID: task.ID, Routed: true},
				}
				if !reflect.DeepEqual(result.Children, want) {
					t.Fatalf("children = %+v, want %+v", result.Children, want)
				}

				after, err := deps.Store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
				if err != nil {
					t.Fatalf("list beads: %v", err)
				}
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("store changed under dry-run:\n got %+v\nwant %+v", after, before)
				}
				if len(runner.calls) != 0 {
					t.Fatalf("runner calls = %#v, want none", runner.calls)
				}
			})
		}
	}
}

// TestDoSlingBatchPlainRoutesExpandedWorkflowRootChild pins that the batch
// refusal, like the single-bead one, is about attaching a formula. A convoy
// slung with no formula in play routes its expanded root child with its
// sibling, attaches nothing, and leaves a molecule the root already carries
// alone; a dry-run counts both children as routable.
func TestDoSlingBatchPlainRoutesExpandedWorkflowRootChild(t *testing.T) {
	plain := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	withDefault := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("code-review")}
	tests := []struct {
		name          string
		target        config.Agent
		noFormula     bool
		force         bool
		dryRun        bool
		wantRouteCmds int
	}{
		{name: "no default formula", target: plain, wantRouteCmds: 2},
		{name: "no default formula/force", target: plain, force: true, wantRouteCmds: 2},
		{name: "no default formula/dry run", target: plain, dryRun: true},
		{name: "no-formula over a default formula", target: withDefault, noFormula: true, wantRouteCmds: 2},
		{name: "no-formula over a default formula/dry run", target: withDefault, noFormula: true, dryRun: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := newFakeRunner()
			cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
			deps := testDeps(cfg, runtime.NewFake(), runner.run)
			convoy, root, task := expandedWorkflowRootConvoyFixture(t, deps.Store)
			wisp, err := deps.Store.Create(beads.Bead{Title: "attached", Type: "molecule", Status: "open"})
			if err != nil {
				t.Fatalf("store.Create(wisp): %v", err)
			}
			if err := deps.Store.SetMetadata(root.ID, beadmeta.MoleculeIDMetadataKey, wisp.ID); err != nil {
				t.Fatalf("SetMetadata(%s molecule_id): %v", root.ID, err)
			}
			before, err := deps.Store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if err != nil {
				t.Fatalf("list beads: %v", err)
			}

			opts := testOpts(tt.target, convoy.ID)
			opts.NoFormula = tt.noFormula
			opts.Force = tt.force
			opts.DryRun = tt.dryRun
			result, err := DoSlingBatch(opts, deps, deps.Store)
			if err != nil {
				t.Fatalf("DoSlingBatch: %v, want a plain batch route of both children", err)
			}
			if result.Method != "batch" {
				t.Fatalf("Method = %q, want batch", result.Method)
			}
			if result.Total != 2 || result.Routed != 2 || result.Failed != 0 {
				t.Fatalf("total=%d routed=%d failed=%d, want total=2 routed=2 failed=0", result.Total, result.Routed, result.Failed)
			}
			if len(result.AutoBurned) != 0 {
				t.Fatalf("AutoBurned = %v, want none on a plain batch", result.AutoBurned)
			}
			if !tt.dryRun {
				want := []SlingChildResult{
					{BeadID: root.ID, Routed: true},
					{BeadID: task.ID, Routed: true},
				}
				if !reflect.DeepEqual(result.Children, want) {
					t.Fatalf("children = %+v, want %+v", result.Children, want)
				}
			}
			if len(runner.calls) != tt.wantRouteCmds {
				t.Fatalf("runner calls = %#v, want %d route calls", runner.calls, tt.wantRouteCmds)
			}
			after, err := deps.Store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if err != nil {
				t.Fatalf("list beads: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("store changed on a plain batch route, which goes through the runner:\n got %+v\nwant %+v", after, before)
			}
		})
	}
}

// TestDoSlingBatchRoutesRootOnlyAndAttemptRootChildren pins that the two bead
// shapes next to an expanded workflow root stay slingable as children of a
// formula-backed batch: a root-only workflow root (the kind without the
// marker) and a retry or ralph attempt root (the marker with gc.kind=task).
// Each gets the formula attached and is routed.
func TestDoSlingBatchRoutesRootOnlyAndAttemptRootChildren(t *testing.T) {
	shapes := []struct {
		title    string
		metadata map[string]string
	}{
		{title: "root-only workflow root", metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}},
		{title: "marked attempt root", metadata: map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindTask,
			beadmeta.WorkflowExpandedMetadataKey: "true",
		}},
	}
	for _, route := range formulaBackedBatchRoutes {
		for _, mode := range formulaBackedBatchModes {
			t.Run(route.name+"/"+mode.name, func(t *testing.T) {
				runner := newFakeRunner()
				cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
				deps := testDeps(cfg, runtime.NewFake(), runner.run)
				convoy, err := deps.Store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
				if err != nil {
					t.Fatalf("store.Create(convoy): %v", err)
				}
				for _, shape := range shapes {
					child, err := deps.Store.Create(beads.Bead{Title: shape.title, Type: "task", Status: "open", Metadata: shape.metadata})
					if err != nil {
						t.Fatalf("store.Create(%s): %v", shape.title, err)
					}
					if err := convoycore.TrackItem(deps.Store, convoy.ID, child.ID); err != nil {
						t.Fatalf("TrackItem(%s): %v", child.ID, err)
					}
				}

				opts := testOpts(route.target, convoy.ID)
				opts.OnFormula = route.onFormula
				opts.Force = mode.force
				result, err := DoSlingBatch(opts, deps, deps.Store)
				if err != nil {
					t.Fatalf("DoSlingBatch: %v", err)
				}
				if result.Routed != len(shapes) || result.Failed != 0 {
					t.Fatalf("routed=%d failed=%d, want routed=%d failed=0", result.Routed, result.Failed, len(shapes))
				}
				for _, child := range result.Children {
					if !child.Routed || child.WispRootID == "" {
						t.Fatalf("child result = %+v, want routed with a wisp attached", child)
					}
					got, err := deps.Store.Get(child.BeadID)
					if err != nil {
						t.Fatalf("store.Get(%s): %v", child.BeadID, err)
					}
					if got.Metadata[beadmeta.MoleculeIDMetadataKey] != child.WispRootID {
						t.Fatalf("%s molecule_id = %q, want %q", child.BeadID, got.Metadata[beadmeta.MoleculeIDMetadataKey], child.WispRootID)
					}
				}
				if len(runner.calls) != len(shapes) {
					t.Fatalf("runner calls = %#v, want %d route calls", runner.calls, len(shapes))
				}
			})
		}
	}
}
