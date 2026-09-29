package runtime

import (
	"errors"
	"testing"
)

func TestMergeBackendListResultsReturnsBestEffortResultsOnPartialFailure(t *testing.T) {
	t.Parallel()

	names, err := MergeBackendListResults(
		BackendListResult{Label: "local", Names: []string{"sess-a"}},
		BackendListResult{Label: "remote", Err: errors.New("backend down")},
	)
	if !IsPartialListError(err) {
		t.Fatalf("MergeBackendListResults() error = %v, want partial list error", err)
	}
	if len(names) != 1 || names[0] != "sess-a" {
		t.Fatalf("MergeBackendListResults() names = %v, want [sess-a]", names)
	}
}

func TestMergeBackendListResultsFailsWhenAllBackendsFail(t *testing.T) {
	t.Parallel()

	names, err := MergeBackendListResults(
		BackendListResult{Label: "local", Err: errors.New("local down")},
		BackendListResult{Label: "remote", Err: errors.New("remote down")},
	)
	if err == nil {
		t.Fatal("MergeBackendListResults() error = nil, want joined error")
	}
	if IsPartialListError(err) {
		t.Fatalf("MergeBackendListResults() error = %v, want total failure not partial", err)
	}
	if names != nil {
		t.Fatalf("MergeBackendListResults() names = %v, want nil", names)
	}
}

func TestMergeBackendListResultsPreservesNamesWhenAllBackendsAreDegraded(t *testing.T) {
	t.Parallel()

	names, err := MergeBackendListResults(
		BackendListResult{Label: "local", Names: []string{"sess-a"}, Err: errors.New("local degraded")},
		BackendListResult{Label: "remote", Names: []string{"sess-b"}, Err: errors.New("remote degraded")},
	)
	if !IsPartialListError(err) {
		t.Fatalf("MergeBackendListResults() error = %v, want partial list error", err)
	}
	if len(names) != 2 || names[0] != "sess-a" || names[1] != "sess-b" {
		t.Fatalf("MergeBackendListResults() names = %v, want [sess-a sess-b]", names)
	}
}

type serverLifecycleFake struct {
	*Fake
	calls []string
	err   error
}

func (f *serverLifecycleFake) ConfigureServer() error {
	f.calls = append(f.calls, "ConfigureServer")
	return f.err
}

func (f *serverLifecycleFake) TeardownServer() error {
	f.calls = append(f.calls, "TeardownServer")
	return f.err
}

func TestServerBackendsFanOutSkipAndJoinErrors(t *testing.T) {
	t.Parallel()

	aErr, bErr := errors.New("a down"), errors.New("b down")
	a := &serverLifecycleFake{Fake: NewFake(), err: aErr}
	b := &serverLifecycleFake{Fake: NewFake(), err: bErr}
	backends := []BackendProvider{
		{Label: "a", Provider: a},
		{Label: "plain", Provider: NewFake()},
		{Label: "b", Provider: b},
	}

	err := ConfigureServerBackends(backends...)
	if !errors.Is(err, aErr) || !errors.Is(err, bErr) {
		t.Fatalf("ConfigureServerBackends() error = %v, want both failures joined", err)
	}
	err = TeardownServerBackends(backends...)
	if !errors.Is(err, aErr) || !errors.Is(err, bErr) {
		t.Fatalf("TeardownServerBackends() error = %v, want both failures joined", err)
	}
	for label, f := range map[string]*serverLifecycleFake{"a": a, "b": b} {
		if len(f.calls) != 2 || f.calls[0] != "ConfigureServer" || f.calls[1] != "TeardownServer" {
			t.Errorf("%s calls = %v, want [ConfigureServer TeardownServer]", label, f.calls)
		}
	}
	if err := TeardownServerBackends(BackendProvider{Label: "plain", Provider: NewFake()}); err != nil {
		t.Fatalf("TeardownServerBackends() with no server-owning backend = %v, want nil", err)
	}
}
