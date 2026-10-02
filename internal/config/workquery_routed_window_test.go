package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// The routed tier reads a bounded window of the reader's priority order and
// then drops the rows the serve rules exclude without a reader flag. An expanded
// workflow root sorts ahead of its own steps, so a route can hold a full window
// of rows the tier drops with routable work behind it. The reconciler's count
// form reads the whole route; the worker's tier has to reach that work too, and
// must not pay for a second read on any route whose first window already
// answers the question.
//
// These tests EXECUTE the generated shell against a reader that honors --limit
// and logs every invocation, so they hold both what is served and which reads
// were issued to serve it.

// routedTierTarget is one route a worker-side query probes with the routed tier.
type routedTierTarget struct {
	name  string
	agent *Agent
	route string
	// firstProbe reports whether route is the first target the query probes, so
	// a result served from its first read is the query's only reader call.
	firstProbe bool
}

func routedTierTargets() []routedTierTarget {
	const (
		controlRoute       = "rig/" + ControlDispatcherAgentName
		legacyControlRoute = "rig/workflow-control"
	)
	return []routedTierTarget{
		{"normal", &Agent{Name: "worker", Dir: "hello-world"}, expandedRootRoute, true},
		{"pool", &Agent{Name: "worker", PoolName: "worker-pool"}, "worker-pool", true},
		{"legacy", &Agent{Name: ControlDispatcherAgentName, Dir: "rig"}, controlRoute, true},
		{"legacy second target", &Agent{Name: ControlDispatcherAgentName, Dir: "rig"}, legacyControlRoute, false},
	}
}

// expandedWorkflowRootRows returns n expanded workflow roots routed to route.
func expandedWorkflowRootRows(n int, route string) []expandedRootRow {
	rows := make([]expandedRootRow, 0, n)
	for i := 1; i <= n; i++ {
		rows = append(rows, expandedWorkflowRootRow(fmt.Sprintf("expanded-root-%02d", i), route))
	}
	return rows
}

// routedStepRows returns n ordinary steps routed to route.
func routedStepRows(n int, route string) []expandedRootRow {
	rows := make([]expandedRootRow, 0, n)
	for i := 1; i <= n; i++ {
		rows = append(rows, routedRow(fmt.Sprintf("routed-step-%02d", i), route, map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindTask,
		}))
	}
	return rows
}

// graphWorkflowRootRows returns n graph.v2 workflow roots routed to route that
// are not expanded: launch work the tier serves, behind executable rows.
func graphWorkflowRootRows(n int, route string) []expandedRootRow {
	rows := make([]expandedRootRow, 0, n)
	for i := 1; i <= n; i++ {
		rows = append(rows, routedRow(fmt.Sprintf("workflow-root-%02d", i), route, map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}))
	}
	return rows
}

func rowIDs(rows []expandedRootRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// routedRouteReader answers the routed read for route with rows, in that order,
// and every other read with nothing.
func routedRouteReader(t *testing.T, route string, rows ...expandedRootRow) string {
	t.Helper()
	if rows == nil {
		rows = []expandedRootRow{}
	}
	return fakePoolDemandReader("[]", fakeReadyArm{glob: routedReadGlob(route), rows: expandedRootRowsJSON(t, rows...)})
}

// unboundedRoutedReadGlob is the sh `case` pattern for the routed read of route
// that asks for every row.
func unboundedRoutedReadGlob(route string) string {
	return routedReadGlob(route) + `"--limit=0"*`
}

// loggedReader returns reader with every invocation appended to the returned
// log file, one "<command word> <argv>" line per call.
func loggedReader(t *testing.T, reader string) (script, log string) {
	t.Helper()
	const shebang = "#!/bin/sh\n"
	if !strings.HasPrefix(reader, shebang) {
		t.Fatalf("fake reader does not start with %q: %q", shebang, reader)
	}
	log = filepath.Join(t.TempDir(), "reader.log")
	return shebang + `printf '%s %s\n' "${0##*/}" "$*" >>` + shellquote.Quote(log) + "\n" + strings.TrimPrefix(reader, shebang), log
}

func readerCalls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read fake reader log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// routedReadCall is the routed tier's read of route as the reader receives it
// on topo, at the given --limit.
func routedReadCall(topo QueryTopology, route string, limit int) string {
	reader := "bd"
	if topo.FederatedReady {
		reader = "gc"
	}
	return reader + " ready" + bdReadyIncludeEphemeralArg(topo.includeEphemeralReady()) +
		" --metadata-field gc.routed_to=" + route +
		" --unassigned --exclude-type=epic --exclude-label hold:mayor --exclude-label hold:external --json --limit=" + strconv.Itoa(limit)
}

// routedReadCalls returns the calls that read route through the routed tier.
func routedReadCalls(calls []string, route string) []string {
	var routed []string
	for _, call := range calls {
		if strings.Contains(call, " --metadata-field gc.routed_to="+route+" ") {
			routed = append(routed, call)
		}
	}
	return routed
}

// windowedRoutedReads is what the routed tier issues for route when its first
// window comes back full of rows it drops: the bounded read, then the whole
// route.
func windowedRoutedReads(topo QueryTopology, route string) []string {
	return []string{
		routedReadCall(topo, route, routedReadyWindow),
		routedReadCall(topo, route, 0),
	}
}

// eachRoutedTierQuery runs fn for every worker-side query kind, agent shape and
// topology that embeds the routed tier.
func eachRoutedTierQuery(t *testing.T, fn func(t *testing.T, target routedTierTarget, topo QueryTopology, command string)) {
	t.Helper()
	for _, target := range routedTierTargets() {
		for _, q := range firstRowQueries() {
			for _, tp := range expandedRootTopologies() {
				t.Run(target.name+"/"+q.name+"/"+tp.name, func(t *testing.T) {
					fn(t, target, tp.topo, q.build(target.agent, tp.topo))
				})
			}
		}
	}
}

// TestRoutedTierServesWorkBehindAWindowOfExpandedRoots pins the read past the
// window: a step behind exactly one window of expanded roots is served, by the
// bounded read followed by a read of the whole route.
func TestRoutedTierServesWorkBehindAWindowOfExpandedRoots(t *testing.T) {
	eachRoutedTierQuery(t, func(t *testing.T, target routedTierTarget, topo QueryTopology, command string) {
		rows := append(expandedWorkflowRootRows(routedReadyWindow, target.route), routedStepRows(1, target.route)...)
		reader, log := loggedReader(t, routedRouteReader(t, target.route, rows...))

		if got, want := servedIDOrder(t, command, reader), []string{"routed-step-01"}; !reflect.DeepEqual(got, want) {
			t.Errorf("routed tier served %v, want %v: the step behind a window of expanded roots was not reached", got, want)
		}
		if got, want := routedReadCalls(readerCalls(t, log), target.route), windowedRoutedReads(topo, target.route); !reflect.DeepEqual(got, want) {
			t.Errorf("routed reads =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	})
}

// TestRoutedTierServesOneWindowOfTheWholeRoute pins the bound on what the read
// of the whole route serves: the first window of servable rows in reader order,
// not every row on the route.
func TestRoutedTierServesOneWindowOfTheWholeRoute(t *testing.T) {
	eachRoutedTierQuery(t, func(t *testing.T, target routedTierTarget, _ QueryTopology, command string) {
		steps := routedStepRows(routedReadyWindow+5, target.route)
		rows := append(expandedWorkflowRootRows(routedReadyWindow, target.route), steps...)

		got := servedIDOrder(t, command, routedRouteReader(t, target.route, rows...))
		if want := rowIDs(steps[:routedReadyWindow]); !reflect.DeepEqual(got, want) {
			t.Errorf("routed tier served %d rows %v, want the first %d servable rows %v", len(got), got, routedReadyWindow, want)
		}
	})
}

// TestRoutedTierCutsTheWholeRouteBeforePreferringExecutableRows pins where the
// window cut sits in the jq pass: the tier keeps the first window of servable
// rows in reader order and moves workflow roots behind executable rows within
// that window. It does not reorder the whole route first, which would serve
// executable rows from beyond the window ahead of the workflow roots at its
// head — an answer the bounded read never gives.
func TestRoutedTierCutsTheWholeRouteBeforePreferringExecutableRows(t *testing.T) {
	for _, tc := range []struct {
		name          string
		workflowRoots int
		steps         int
		want          func(roots, steps []expandedRootRow) []string
	}{
		{
			name:          "more workflow roots than the window holds, steps behind them",
			workflowRoots: routedReadyWindow + 2,
			steps:         3,
			want: func(roots, _ []expandedRootRow) []string {
				return rowIDs(roots[:routedReadyWindow])
			},
		},
		{
			name:          "workflow roots and the steps behind them inside one window",
			workflowRoots: routedReadyWindow - 3,
			steps:         3,
			want: func(roots, steps []expandedRootRow) []string {
				return append(rowIDs(steps), rowIDs(roots)...)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachRoutedTierQuery(t, func(t *testing.T, target routedTierTarget, _ QueryTopology, command string) {
				roots := graphWorkflowRootRows(tc.workflowRoots, target.route)
				steps := routedStepRows(tc.steps, target.route)
				rows := append(expandedWorkflowRootRows(routedReadyWindow, target.route), roots...)
				rows = append(rows, steps...)

				got := servedIDOrder(t, command, routedRouteReader(t, target.route, rows...))
				if want := tc.want(roots, steps); !reflect.DeepEqual(got, want) {
					t.Errorf("routed tier served %v, want %v", got, want)
				}
			})
		})
	}
}

// TestRoutedTierServesFromTheFirstWindowWithOneRead pins the path every route
// without a full window of dropped rows takes: the bounded read is the only
// routed read, whether it serves work or shows there is none.
func TestRoutedTierServesFromTheFirstWindowWithOneRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows func(route string) []expandedRootRow
		want func(route string) []string
	}{
		{
			name: "a step behind one fewer expanded root than the window holds",
			rows: func(route string) []expandedRootRow {
				return append(expandedWorkflowRootRows(routedReadyWindow-1, route), routedStepRows(1, route)...)
			},
			want: func(string) []string { return []string{"routed-step-01"} },
		},
		{
			name: "more steps than the window holds",
			rows: func(route string) []expandedRootRow { return routedStepRows(routedReadyWindow+5, route) },
			want: func(route string) []string { return rowIDs(routedStepRows(routedReadyWindow, route)) },
		},
		{
			name: "expanded roots alone, one fewer than the window holds",
			rows: func(route string) []expandedRootRow { return expandedWorkflowRootRows(routedReadyWindow-1, route) },
			want: func(string) []string { return []string{} },
		},
		{
			name: "an empty route",
			rows: func(string) []expandedRootRow { return nil },
			want: func(string) []string { return []string{} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachRoutedTierQuery(t, func(t *testing.T, target routedTierTarget, topo QueryTopology, command string) {
				reader, log := loggedReader(t, routedRouteReader(t, target.route, tc.rows(target.route)...))
				want := tc.want(target.route)

				if got := servedIDOrder(t, command, reader); !reflect.DeepEqual(got, want) {
					t.Errorf("routed tier served %v, want %v", got, want)
				}
				calls := readerCalls(t, log)
				boundedRead := []string{routedReadCall(topo, target.route, routedReadyWindow)}
				if got := routedReadCalls(calls, target.route); !reflect.DeepEqual(got, boundedRead) {
					t.Errorf("routed reads =\n  %s\nwant the bounded read alone\n  %s", strings.Join(got, "\n  "), boundedRead[0])
				}
				if len(want) > 0 && target.firstProbe && !reflect.DeepEqual(calls, boundedRead) {
					t.Errorf("reader calls =\n  %s\nwant the bounded read alone\n  %s", strings.Join(calls, "\n  "), boundedRead[0])
				}
			})
		})
	}
}

// TestRoutedTierWindowOfExpandedRootsStillFallsThrough pins the empty outcome of
// the read past the window: a route holding nothing but expanded roots, more
// than one window of them, is read whole, serves nothing, and leaves the probe
// to the tiers behind it — the legacy gc.run_target tier, the ephemeral tier and
// a second target.
func TestRoutedTierWindowOfExpandedRootsStillFallsThrough(t *testing.T) {
	const (
		controlRoute       = "rig/" + ControlDispatcherAgentName
		legacyControlRoute = "rig/workflow-control"
	)
	onlyExpandedRoots := func(route string) fakeReadyArm {
		return fakeReadyArm{
			glob: routedReadGlob(route),
			rows: expandedRootRowsJSON(t, expandedWorkflowRootRows(routedReadyWindow+5, route)...),
		}
	}
	ephemeralStep := expandedRootRow{
		ID: "ephemeral-step", Status: "open", CreatedAt: "2026-05-01T00:00:00Z",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: expandedRootRoute},
	}

	for _, tc := range []struct {
		name      string
		agent     *Agent
		route     string
		ephemeral bool
		reader    string
		want      []string
	}{
		{
			name:   "nothing behind it",
			agent:  &Agent{Name: "worker", Dir: "hello-world"},
			route:  expandedRootRoute,
			reader: fakePoolDemandReader("[]", onlyExpandedRoots(expandedRootRoute)),
			want:   []string{},
		},
		{
			name:  "legacy run_target tier",
			agent: &Agent{Name: "worker", Dir: "hello-world"},
			route: expandedRootRoute,
			reader: fakePoolDemandReader("[]",
				onlyExpandedRoots(expandedRootRoute),
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
			want: []string{"legacy-root"},
		},
		{
			name:      "ephemeral tier",
			agent:     &Agent{Name: "worker", Dir: "hello-world"},
			route:     expandedRootRoute,
			ephemeral: true,
			reader:    fakePoolDemandReader(expandedRootRowsJSON(t, ephemeralStep), onlyExpandedRoots(expandedRootRoute)),
			want:      []string{"ephemeral-step"},
		},
		{
			name:  "second target",
			agent: &Agent{Name: ControlDispatcherAgentName, Dir: "rig"},
			route: controlRoute,
			reader: fakePoolDemandReader("[]",
				onlyExpandedRoots(controlRoute),
				fakeReadyArm{
					glob: routedReadGlob(legacyControlRoute),
					rows: expandedRootRowsJSON(t, routedRow("second-target-step", legacyControlRoute, nil)),
				}),
			want: []string{"second-target-step"},
		},
	} {
		for _, q := range firstRowQueries() {
			for _, tp := range expandedRootTopologies() {
				if tc.ephemeral && tp.topo.includeEphemeralReady() {
					continue
				}
				t.Run(tc.name+"/"+q.name+"/"+tp.name, func(t *testing.T) {
					reader, log := loggedReader(t, tc.reader)

					if got := servedIDOrder(t, q.build(tc.agent, tp.topo), reader); !reflect.DeepEqual(got, tc.want) {
						t.Errorf("work query served %v, want %v: a route of expanded roots ended the probe", got, tc.want)
					}
					if got, want := routedReadCalls(readerCalls(t, log), tc.route), windowedRoutedReads(tp.topo, tc.route); !reflect.DeepEqual(got, want) {
						t.Errorf("routed reads =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
					}
				})
			}
		}
	}
}

// TestRoutedTierReadOfTheWholeRouteKeepsTheFailureDiscipline pins the read past
// the window to the bounded read's own failure handling. A failed read is never
// spent as "no work" where the reader is the federated one — the query exits
// with the reader's status and its stderr reaches the caller — and falls through
// to the later tiers on a single store. A payload jq cannot parse is served
// exactly as the reader printed it.
func TestRoutedTierReadOfTheWholeRouteKeepsTheFailureDiscipline(t *testing.T) {
	legacyRoot := func(route string) fakeReadyArm {
		return fakeReadyArm{
			glob: runTargetReadGlob(route),
			rows: expandedRootRowsJSON(t, expandedRootRow{
				ID: "legacy-root", Status: "open",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:      beadmeta.KindWorkflow,
					beadmeta.RunTargetMetadataKey: route,
				},
			}),
		}
	}
	windowOfExpandedRoots := func(route string) fakeReadyArm {
		return fakeReadyArm{
			glob: routedReadGlob(route),
			rows: expandedRootRowsJSON(t, expandedWorkflowRootRows(routedReadyWindow, route)...),
		}
	}

	t.Run("the read fails", func(t *testing.T) {
		eachRoutedTierQuery(t, func(t *testing.T, target routedTierTarget, topo QueryTopology, command string) {
			reader, log := loggedReader(t, fakePoolDemandReader("[]",
				fakeReadyArm{glob: unboundedRoutedReadGlob(target.route), fails: true},
				windowOfExpandedRoots(target.route),
				legacyRoot(target.route)))

			res := runPoolDemandQuery(t, command, reader)
			if got, want := routedReadCalls(readerCalls(t, log), target.route), windowedRoutedReads(topo, target.route); !reflect.DeepEqual(got, want) {
				t.Fatalf("routed reads =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			if topo.FederatedReady {
				if res.exit != fakeReaderFailureStatus {
					t.Errorf("work query exited %d over a failed federated read, want the reader's own status %d (stdout=%q)", res.exit, fakeReaderFailureStatus, res.stdout)
				}
				if res.stdout != "" {
					t.Errorf("work query printed %q over a failed federated read; a read that failed must not be answered", res.stdout)
				}
				if !strings.Contains(res.stderr, fakeReaderFailureMessage) {
					t.Errorf("stderr = %q, want the federated reader's own message %q", res.stderr, fakeReaderFailureMessage)
				}
				return
			}
			if res.exit != 0 {
				t.Fatalf("work query exited %d over a failed single-store read (stderr=%q)", res.exit, res.stderr)
			}
			if got, want := workQueryOutputIDOrder(t, res.stdout), []string{"legacy-root"}; !reflect.DeepEqual(got, want) {
				t.Errorf("work query served %v, want %v: a failed single-store read falls through to the later tiers", got, want)
			}
			if res.stderr != "" {
				t.Errorf("stderr = %q, want the single-store reader's chatter discarded", res.stderr)
			}
		})
	})

	t.Run("the payload is unparseable", func(t *testing.T) {
		eachRoutedTierQuery(t, func(t *testing.T, target routedTierTarget, _ QueryTopology, command string) {
			reader := fakePoolDemandReader("[]",
				fakeReadyArm{glob: unboundedRoutedReadGlob(target.route), rows: unparseableRoutedPayload},
				windowOfExpandedRoots(target.route),
				legacyRoot(target.route))

			res := runPoolDemandQuery(t, command, reader)
			if res.exit != 0 {
				t.Fatalf("work query exited %d over an unparseable routed payload (stderr=%q)", res.exit, res.stderr)
			}
			if res.stdout != unparseableRoutedPayload {
				t.Errorf("unparseable routed payload was not served unchanged: got %q, want %q", res.stdout, unparseableRoutedPayload)
			}
		})
	})
}

// TestWorkerQueryServesWheneverTheCountFormReportsRoutedDemand pins the two
// forms of the routed tier to each other across the window boundary: the worker
// query serves a row exactly when the reconciler's count form, which reads the
// whole route, counts one.
func TestWorkerQueryServesWheneverTheCountFormReportsRoutedDemand(t *testing.T) {
	a := &Agent{Name: "worker", Dir: "hello-world"}
	const route = expandedRootRoute
	roots, steps := expandedWorkflowRootRows, routedStepRows

	for _, tc := range []struct {
		name      string
		rows      []expandedRootRow
		wantCount string
	}{
		{"an empty route", nil, "0"},
		{"one fewer expanded root than the window holds", roots(routedReadyWindow-1, route), "0"},
		{"a step behind one fewer expanded root than the window holds", append(roots(routedReadyWindow-1, route), steps(1, route)...), "1"},
		{"a window of expanded roots", roots(routedReadyWindow, route), "0"},
		{"a step behind a window of expanded roots", append(roots(routedReadyWindow, route), steps(1, route)...), "1"},
		{"more expanded roots than the window holds", roots(routedReadyWindow+5, route), "0"},
		{"steps behind more expanded roots than the window holds", append(roots(routedReadyWindow+5, route), steps(3, route)...), "3"},
		{"more steps than the window holds", steps(routedReadyWindow+5, route), strconv.Itoa(routedReadyWindow + 5)},
	} {
		reader := routedRouteReader(t, route, tc.rows...)
		for _, tp := range expandedRootTopologies() {
			count := demandCount(t, a.EffectivePoolDemandQueryFor(tp.topo), reader)
			if count != tc.wantCount {
				t.Errorf("%s/%s: pool-demand count = %q, want %q", tc.name, tp.name, count, tc.wantCount)
			}
			for _, q := range firstRowQueries() {
				t.Run(tc.name+"/"+q.name+"/"+tp.name, func(t *testing.T) {
					served := servedIDOrder(t, q.build(a, tp.topo), reader)
					if (count != "0") != (len(served) > 0) {
						t.Fatalf("count form reports %s routed rows of demand, worker query served %v", count, served)
					}
				})
			}
		}
	}
}
