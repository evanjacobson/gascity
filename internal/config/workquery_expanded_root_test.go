package config

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// An expanded workflow root (beadmeta.IsExpandedWorkflowRoot) is a container:
// its child steps are the work. The routed pool-demand query must neither serve
// it to a worker nor count it as demand, and — because each tier of the worker
// query returns the moment it holds a non-empty result — must not let it stand
// in front of the work in the tiers behind it.
//
// These tests EXECUTE the generated shell against a fake reader. The rule lives
// in the query's jq, which no reader flag expresses, so a test that inspects the
// reader's flags or evaluates only the Go predicate cannot see it.

const expandedRootRoute = "hello-world/worker"

// expandedRootRow is one routed bead as a reader would emit it.
type expandedRootRow struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Assignee  string            `json:"assignee"`
	CreatedAt string            `json:"created_at,omitempty"`
	Metadata  map[string]string `json:"metadata"`
}

func expandedRootRowsJSON(t *testing.T, rows ...expandedRootRow) string {
	t.Helper()
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("encode fake reader rows: %v", err)
	}
	return string(encoded)
}

func routedRow(id, route string, meta map[string]string) expandedRootRow {
	metadata := map[string]string{beadmeta.RoutedToMetadataKey: route}
	for k, v := range meta {
		metadata[k] = v
	}
	return expandedRootRow{ID: id, Status: "open", Metadata: metadata}
}

func expandedWorkflowRootRow(id, route string) expandedRootRow {
	return routedRow(id, route, map[string]string{
		beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
		beadmeta.WorkflowExpandedMetadataKey: "true",
		beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
	})
}

// expandedRootCorpus is the four routed shapes the rule has to tell apart, in
// the order a reader would return them with the container first. Values are
// canonical — exact "true", exact kinds — because jq compares exactly where the
// Go predicate trims.
func expandedRootCorpus(route string) []expandedRootRow {
	return []expandedRootRow{
		expandedWorkflowRootRow("expanded-root", route),
		routedRow("root-only", route, map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}),
		routedRow("attempt-root", route, map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindTask,
			beadmeta.WorkflowExpandedMetadataKey: "true",
		}),
		routedRow("ready-step", route, map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindTask,
			beadmeta.RootBeadIDMetadataKey: "expanded-root",
		}),
	}
}

// booleanMarkerRowsJSON encodes rows the way a store that holds the expanded
// marker as a JSON boolean returns them: every "true" marker becomes true, and a
// copy of the root-only root, "false-marker-root", is added carrying false. The
// Go predicate reads metadata through a string map that renders a boolean as
// its text, so these rows mean to it exactly what the string corpus does.
func booleanMarkerRowsJSON(t *testing.T, rows []expandedRootRow) string {
	t.Helper()
	rows = slices.Clone(rows)
	rootOnly := slices.IndexFunc(rows, func(row expandedRootRow) bool { return row.ID == "root-only" })
	if rootOnly < 0 {
		t.Fatal("corpus has no root-only row to derive the false-marker root from")
	}
	falseMarkerRoot := rows[rootOnly]
	falseMarkerRoot.ID = "false-marker-root"
	falseMarkerRoot.Metadata = maps.Clone(falseMarkerRoot.Metadata)
	falseMarkerRoot.Metadata[beadmeta.WorkflowExpandedMetadataKey] = "false"
	rows = append(rows, falseMarkerRoot)

	encoded := expandedRootRowsJSON(t, rows...)
	marker := `"` + beadmeta.WorkflowExpandedMetadataKey + `":`
	for _, value := range []string{"true", "false"} {
		if !strings.Contains(encoded, marker+`"`+value+`"`) {
			t.Fatalf("corpus carries no %s marker to re-encode as a boolean: %s", value, encoded)
		}
		encoded = strings.ReplaceAll(encoded, marker+`"`+value+`"`, marker+value)
	}
	return encoded
}

// fakeReadyArm answers the ready reads whose argv matches glob with rows.
type fakeReadyArm struct {
	glob string
	rows string
}

// runTargetReadGlob is the sh `case` pattern for the legacy tier's read, the
// gc.run_target counterpart of routedReadGlob.
func runTargetReadGlob(route string) string {
	return `*"--metadata-field gc.run_target=` + route + `"*`
}

// fakePoolDemandReader is a reader stand-in whose pool-demand reads are answered
// independently: each ready arm in order, then the open ephemeral scan. Every
// other read is empty. It serves as `bd` and as `gc` alike — both take `ready`
// as their first argument — so one fake covers the single-store and federated
// forms of a query.
func fakePoolDemandReader(ephemeralOpenRows string, ready ...fakeReadyArm) string {
	var arms strings.Builder
	for _, arm := range ready {
		arms.WriteString("      " + arm.glob + ")\n        printf '%s' '" + arm.rows + "'\n        ;;\n")
	}
	return `#!/bin/sh
set -eu
case "$1" in
  ready)
    case "$*" in
` + arms.String() + `      *)
        printf '[]'
        ;;
    esac
    ;;
  query)
    case "$*" in
      *"ephemeral=true AND status=open"*)
        printf '%s' '` + ephemeralOpenRows + `'
        ;;
      *)
        printf '[]'
        ;;
    esac
    ;;
  *)
    printf '[]'
    ;;
esac
`
}

// expandedRootTopology is one row of the golden files' topology axis.
type expandedRootTopology struct {
	name string
	topo QueryTopology
}

func expandedRootTopologies() []expandedRootTopology {
	return []expandedRootTopology{
		{"bd104", QueryTopology{}},
		{"bd105", singleStoreTopology()},
		{"bd104_federated", QueryTopology{FederatedReady: true}},
		{"bd105_federated", federatedTopology()},
	}
}

// firstRowQuery is a worker-side query that embeds the routed first-row tier.
type firstRowQuery struct {
	name  string
	build func(*Agent, QueryTopology) string
}

func firstRowQueries() []firstRowQuery {
	return []firstRowQuery{
		{"Work", (*Agent).EffectiveWorkQueryFor},
		{"RoutedPool", (*Agent).EffectiveRoutedPoolQueryFor},
	}
}

// runPoolDemandQuery runs a generated query with reader installed as both `bd`
// and `gc`, as a pool seat with no claim of its own.
func runPoolDemandQuery(t *testing.T, command, reader string) generatedQueryResult {
	t.Helper()
	requireJQ(t)
	return runGeneratedQueryWithBD(t, command, map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
	}, reader, reader)
}

func servedIDOrder(t *testing.T, command, reader string) []string {
	t.Helper()
	res := runPoolDemandQuery(t, command, reader)
	if res.exit != 0 {
		t.Fatalf("work query exited %d (stderr=%q)", res.exit, res.stderr)
	}
	return workQueryOutputIDOrder(t, res.stdout)
}

func demandCount(t *testing.T, command, reader string) string {
	t.Helper()
	res := runPoolDemandQuery(t, command, reader)
	if res.exit != 0 {
		t.Fatalf("pool-demand query exited %d (stderr=%q)", res.exit, res.stderr)
	}
	return strings.TrimSpace(res.stdout)
}

// TestExpandedRootCorpusMatchesGoPredicate ties the corpus to the Go form of the
// rule: the one row the query tests below expect dropped is exactly the row
// beadmeta.IsExpandedWorkflowRoot names, and the three they expect kept are the
// rows it does not.
func TestExpandedRootCorpusMatchesGoPredicate(t *testing.T) {
	for _, row := range expandedRootCorpus(expandedRootRoute) {
		want := row.ID == "expanded-root"
		if got := beadmeta.IsExpandedWorkflowRoot(row.Metadata); got != want {
			t.Errorf("IsExpandedWorkflowRoot(%s) = %v, want %v", row.ID, got, want)
		}
	}
}

// TestRoutedTierDropsExpandedWorkflowRootAndKeepsWork pins the routed read: the
// expanded root is not served, and the root-only workflow root, the marked
// attempt root (gc.kind=task) and the ordinary step all still are.
func TestRoutedTierDropsExpandedWorkflowRootAndKeepsWork(t *testing.T) {
	a := &Agent{Name: "worker", Dir: "hello-world"}
	reader := fakePoolDemandReader("[]", fakeReadyArm{
		glob: routedReadGlob(expandedRootRoute),
		rows: expandedRootRowsJSON(t, expandedRootCorpus(expandedRootRoute)...),
	})
	// Executable work first, the root-only launch fallback behind it.
	want := []string{"attempt-root", "ready-step", "root-only"}

	for _, q := range firstRowQueries() {
		for _, tp := range expandedRootTopologies() {
			t.Run(q.name+"/"+tp.name, func(t *testing.T) {
				got := servedIDOrder(t, q.build(a, tp.topo), reader)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("routed tier served %v, want %v (the expanded root dropped, every other shape kept)", got, want)
				}
			})
		}
	}
}

// TestExpandedWorkflowRootDoesNotHideLaterTiers pins the consequence a rule
// applied only after the query returns cannot deliver. The routed tier returns
// as soon as it holds a non-empty result, so an expanded root that is the only
// row in it ends the query there — and the work behind it, in the legacy
// gc.run_target tier, the ephemeral tier or a second target, is never read.
func TestExpandedWorkflowRootDoesNotHideLaterTiers(t *testing.T) {
	const (
		controlRoute       = "rig/" + ControlDispatcherAgentName
		legacyControlRoute = "rig/workflow-control"
	)
	onlyExpandedRoot := func(route string) fakeReadyArm {
		return fakeReadyArm{
			glob: routedReadGlob(route),
			rows: expandedRootRowsJSON(t, expandedWorkflowRootRow("expanded-root", route)),
		}
	}
	ephemeralStep := expandedRootRow{
		ID: "ephemeral-step", Status: "open", CreatedAt: "2026-05-01T00:00:00Z",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: expandedRootRoute},
	}

	for _, tc := range []struct {
		name      string
		agent     *Agent
		ephemeral bool
		reader    string
		want      string
	}{
		{
			name:  "legacy run_target tier",
			agent: &Agent{Name: "worker", Dir: "hello-world"},
			reader: fakePoolDemandReader("[]",
				onlyExpandedRoot(expandedRootRoute),
				fakeReadyArm{
					glob: runTargetReadGlob(expandedRootRoute),
					rows: expandedRootRowsJSON(t, expandedRootRow{
						ID: "legacy-root", Status: "open",
						Metadata: map[string]string{
							beadmeta.KindMetadataKey:      beadmeta.KindWorkflow,
							beadmeta.RunTargetMetadataKey: expandedRootRoute,
						},
					}),
				}),
			want: "legacy-root",
		},
		{
			name:      "ephemeral tier",
			agent:     &Agent{Name: "worker", Dir: "hello-world"},
			ephemeral: true,
			reader: fakePoolDemandReader(expandedRootRowsJSON(t, ephemeralStep),
				onlyExpandedRoot(expandedRootRoute)),
			want: "ephemeral-step",
		},
		{
			name:  "second target",
			agent: &Agent{Name: ControlDispatcherAgentName, Dir: "rig"},
			reader: fakePoolDemandReader("[]",
				onlyExpandedRoot(controlRoute),
				fakeReadyArm{
					glob: routedReadGlob(legacyControlRoute),
					rows: expandedRootRowsJSON(t, routedRow("second-target-step", legacyControlRoute, nil)),
				}),
			want: "second-target-step",
		},
		{
			// The ephemeral selector is its own first-row read: it sorts oldest
			// first and serves one row, so an older expanded root in the wisp
			// tier hides the step behind it the same way.
			name:      "ephemeral tier's own expanded root",
			agent:     &Agent{Name: "worker", Dir: "hello-world"},
			ephemeral: true,
			reader: fakePoolDemandReader(expandedRootRowsJSON(t,
				func() expandedRootRow {
					root := expandedWorkflowRootRow("ephemeral-expanded-root", expandedRootRoute)
					root.CreatedAt = "2026-04-01T00:00:00Z"
					return root
				}(),
				ephemeralStep,
			)),
			want: "ephemeral-step",
		},
	} {
		for _, q := range firstRowQueries() {
			for _, tp := range expandedRootTopologies() {
				if tc.ephemeral && tp.topo.includeEphemeralReady() {
					continue
				}
				t.Run(tc.name+"/"+q.name+"/"+tp.name, func(t *testing.T) {
					got := servedIDOrder(t, q.build(tc.agent, tp.topo), tc.reader)
					if !reflect.DeepEqual(got, []string{tc.want}) {
						t.Fatalf("work query served %v, want [%s]: an expanded workflow root hid the work behind it", got, tc.want)
					}
				})
			}
		}
	}
}

// TestPoolDemandCountSkipsExpandedWorkflowRoot pins the reconciler count form:
// an expanded root is not demand, in the routed tier or the ephemeral one, and
// the three kept shapes still are.
func TestPoolDemandCountSkipsExpandedWorkflowRoot(t *testing.T) {
	a := &Agent{Name: "worker", Dir: "hello-world"}
	corpus := expandedRootRowsJSON(t, expandedRootCorpus(expandedRootRoute)...)

	for _, tc := range []struct {
		name      string
		ephemeral bool
		reader    string
		want      string
	}{
		{
			name:   "routed tier",
			reader: fakePoolDemandReader("[]", fakeReadyArm{glob: routedReadGlob(expandedRootRoute), rows: corpus}),
			want:   "3",
		},
		{
			name: "routed tier holding only an expanded root",
			reader: fakePoolDemandReader("[]", fakeReadyArm{
				glob: routedReadGlob(expandedRootRoute),
				rows: expandedRootRowsJSON(t, expandedWorkflowRootRow("expanded-root", expandedRootRoute)),
			}),
			want: "0",
		},
		{
			name:      "ephemeral tier",
			ephemeral: true,
			reader:    fakePoolDemandReader(corpus),
			want:      "3",
		},
	} {
		for _, tp := range expandedRootTopologies() {
			if tc.ephemeral && tp.topo.includeEphemeralReady() {
				continue
			}
			t.Run(tc.name+"/"+tp.name, func(t *testing.T) {
				if got := demandCount(t, a.EffectivePoolDemandQueryFor(tp.topo), tc.reader); got != tc.want {
					t.Fatalf("pool-demand count = %q, want %q (an expanded workflow root is not demand; a root-only root, a marked attempt root and a step are)", got, tc.want)
				}
			})
		}
	}
}

// TestPoolDemandQueryReadsABooleanExpandedMarker pins the marker's encoding out
// of the rule: a store may hold gc.workflow_expanded as the JSON boolean true
// rather than the string "true", and the query must read the two alike. A
// workflow root marked true is dropped and not counted; a marked attempt root
// (gc.kind=task) and a workflow root marked false are still served and counted.
func TestPoolDemandQueryReadsABooleanExpandedMarker(t *testing.T) {
	a := &Agent{Name: "worker", Dir: "hello-world"}
	reader := fakePoolDemandReader("[]", fakeReadyArm{
		glob: routedReadGlob(expandedRootRoute),
		rows: booleanMarkerRowsJSON(t, expandedRootCorpus(expandedRootRoute)),
	})
	// Executable work first, the two servable workflow roots behind it.
	wantServed := []string{"attempt-root", "ready-step", "root-only", "false-marker-root"}
	const wantCount = "4"

	for _, tp := range expandedRootTopologies() {
		for _, q := range firstRowQueries() {
			t.Run(q.name+"/"+tp.name, func(t *testing.T) {
				got := servedIDOrder(t, q.build(a, tp.topo), reader)
				if !reflect.DeepEqual(got, wantServed) {
					t.Fatalf("routed tier served %v, want %v (a boolean-true marker on a workflow root is an expanded root)", got, wantServed)
				}
			})
		}
		t.Run("PoolDemand/"+tp.name, func(t *testing.T) {
			if got := demandCount(t, a.EffectivePoolDemandQueryFor(tp.topo), reader); got != wantCount {
				t.Fatalf("pool-demand count = %q, want %q (a boolean-true marker on a workflow root is an expanded root)", got, wantCount)
			}
		})
	}
}

// unparseableRoutedPayload is a reader payload jq cannot parse: a diagnostic
// line ahead of the array.
const unparseableRoutedPayload = `warning: store compacted
[{"id":"routed-step"}]`

// TestRoutedTierServesUnparseableReaderPayloadUnchanged pins the worker-side
// failure discipline around the jq rule: a payload jq cannot parse is handed to
// the hook exactly as the reader printed it, never rewritten into an empty
// result that reads as "no work".
func TestRoutedTierServesUnparseableReaderPayloadUnchanged(t *testing.T) {
	a := &Agent{Name: "worker", Dir: "hello-world"}
	reader := fakePoolDemandReader("[]", fakeReadyArm{
		glob: routedReadGlob(expandedRootRoute),
		rows: unparseableRoutedPayload,
	})

	for _, q := range firstRowQueries() {
		for _, tp := range expandedRootTopologies() {
			t.Run(q.name+"/"+tp.name, func(t *testing.T) {
				res := runPoolDemandQuery(t, q.build(a, tp.topo), reader)
				if res.exit != 0 {
					t.Fatalf("work query exited %d over an unparseable routed payload (stderr=%q)", res.exit, res.stderr)
				}
				if res.stdout != unparseableRoutedPayload {
					t.Fatalf("unparseable routed payload was not served unchanged: got %q, want %q", res.stdout, unparseableRoutedPayload)
				}
			})
		}
	}
}

// TestPoolDemandCountDoesNotDefaultToZeroWhenItCannotCount pins the count form's
// failure discipline around the jq rule: a ready read that fails, or one whose
// payload jq cannot parse, is an error — never a count of 0, which is what stops
// a pool from spawning.
func TestPoolDemandCountDoesNotDefaultToZeroWhenItCannotCount(t *testing.T) {
	a := &Agent{Name: "worker", Dir: "hello-world"}

	// Only the routed read misbehaves; every other read answers "[]". A reader
	// that failed everywhere would be caught by the legacy tier's own failure
	// clause and prove nothing about the routed one.
	routedReadFails := `#!/bin/sh
case "$*" in
  ` + routedReadGlob(expandedRootRoute) + `)
    printf 'reader: store unreadable\n' >&2
    exit 1
    ;;
  *)
    printf '[]'
    ;;
esac
`

	for _, tc := range []struct {
		name   string
		reader string
	}{
		{"routed read fails", routedReadFails},
		{"routed payload is unparseable", fakePoolDemandReader("[]", fakeReadyArm{
			glob: routedReadGlob(expandedRootRoute),
			rows: unparseableRoutedPayload,
		})},
	} {
		for _, tp := range expandedRootTopologies() {
			t.Run(tc.name+"/"+tp.name, func(t *testing.T) {
				res := runPoolDemandQuery(t, a.EffectivePoolDemandQueryFor(tp.topo), tc.reader)
				if res.exit == 0 {
					t.Fatalf("pool-demand query exited 0 and printed %q; a count it could not compute must be an error", res.stdout)
				}
				if strings.TrimSpace(res.stdout) != "" {
					t.Errorf("pool-demand query printed %q before failing; a failed count must not print a number", res.stdout)
				}
			})
		}
	}
}

// expandedRootSelectClause is the jq PoolDemandServeRules renders for
// ExcludeExpandedWorkflowRoots, spelled out so the pin holds the program rather
// than the helpers that build it: kind AND marker, never the marker alone.
const expandedRootSelectClause = ` | select((((.metadata["gc.kind"] // "") == "workflow") and (((.metadata["gc.workflow_expanded"] // "") | tostring) == "true")) | not)`

// TestPoolDemandServeRulesJQSelectClauses pins the descriptor as the jq rule's
// source: the clause is rendered from the field, and a descriptor that does not
// declare the rule renders nothing.
func TestPoolDemandServeRulesJQSelectClauses(t *testing.T) {
	if got := (PoolDemandServeRules{ExcludeExpandedWorkflowRoots: true}).JQSelectClauses(); got != expandedRootSelectClause {
		t.Errorf("JQSelectClauses() with ExcludeExpandedWorkflowRoots = %q, want %q", got, expandedRootSelectClause)
	}
	if got := (PoolDemandServeRules{}).JQSelectClauses(); got != "" {
		t.Errorf("JQSelectClauses() of a descriptor declaring no jq rule = %q, want none", got)
	}
	if got := PoolDemandServeRulesForQuery().JQSelectClauses(); got != expandedRootSelectClause {
		t.Errorf("PoolDemandServeRulesForQuery().JQSelectClauses() = %q, want %q", got, expandedRootSelectClause)
	}
}

// TestExpandedRootRuleIsRenderedOncePerPoolDemandRead pins where the rule is
// applied: once on the routed read, and once in the ephemeral selector on the
// topologies that have an ephemeral tier — in the worker's first-row form and
// the reconciler's count form alike. The legacy gc.run_target tier keeps its own
// filter and carries no copy.
func TestExpandedRootRuleIsRenderedOncePerPoolDemandRead(t *testing.T) {
	kinds := append(firstRowQueries(), firstRowQuery{"PoolDemand", (*Agent).EffectivePoolDemandQueryFor})
	for _, shape := range []parityShape{
		{"normal", &Agent{Name: "worker"}},
		{"legacy", &Agent{Name: ControlDispatcherAgentName, Dir: "rig"}},
	} {
		for _, q := range kinds {
			for _, tp := range expandedRootTopologies() {
				want := 2
				if tp.topo.includeEphemeralReady() {
					want = 1
				}
				query := q.build(shape.agent, tp.topo)
				if got := strings.Count(query, expandedRootSelectClause); got != want {
					t.Errorf("%s/%s/%s: the expanded-root rule is rendered %d times, want %d", shape.name, q.name, tp.name, got, want)
				}
			}
		}
	}
}
