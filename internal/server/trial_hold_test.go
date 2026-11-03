package server

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iconidentify/maclab/internal/api"
)

// Only a baseline a Maintainer submits after a hold clears it: one queued
// before it (behind the Trial run that set it) is refused when its turn
// comes, and so is any retry of it; a new one runs and clears the hold.
func TestOnlyABaselineSubmittedAfterTheHoldClearsIt(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	l.sim.SetTrialLeak(true)
	trial := l.submitJob(api.JobSpec{Trial: true, Tests: []api.TestSpec{l.trialScript("migrate")}})
	early := l.submitJob(api.JobSpec{Baseline: true})
	l.expect(l.jobAPI(trial, 60*time.Second), api.OutcomeInfra)
	got := l.jobAPI(early, 60*time.Second)
	if got.Outcome != api.OutcomeCanceled || !strings.Contains(got.Summary, "held after a Trial run") || got.Result.Booted {
		t.Fatalf("a baseline queued before the hold: %s %q", got.Outcome, got.Summary)
	}
	if d := l.s.dev(l.name).snapshot(); d.TrialHold == "" || d.State == api.StateReady {
		t.Fatalf("the early baseline cleared the hold: %s %q", d.State, d.TrialHold)
	}
	if _, code, err := l.s.retry(harnessMaintainer, early, ""); err == nil || code != 409 {
		t.Fatalf("a retry of the early baseline: %d %v", code, err)
	}
	l.sim.SetTrialLeak(false)
	l.baseline()
	if d := l.s.dev(l.name).snapshot(); d.TrialHold != "" || !d.TrialHoldSince.IsZero() || d.State != api.StateReady {
		t.Fatalf("after a baseline submitted after the hold: %s, hold %q", d.State, d.TrialHold)
	}
}

// labd dying after it saved a Trial run's changed after-check but before it
// held the Mac: the restart holds it from the saved result. A run whose
// saved check came out unchanged holds nothing.
func TestLabdRestartAfterAChangedCheckHoldsTheMac(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{DataDir: t.TempDir(), FirstMaintainer: harnessMaintainer, FirstMaintainerToken: "a"}
	quiet := log.New(io.Discard, "", 0)
	s1, err := New(ctx, cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	own := &api.OwnSystem{Root: "/@", Digests: map[string]string{"packages": "x"}}
	changed := &api.OwnSystem{Root: "/@", Digests: map[string]string{"packages": "y"}}
	for _, c := range []struct {
		mac, job string
		after    *api.OwnSystem
		same     bool
	}{
		{"m-changed", "j1102-000000-aaaa", changed, false},
		{"m-same", "j1102-000000-bbbb", own, true},
	} {
		d := api.Device{ID: c.mac, Name: c.mac, State: api.StateBusy, KnownGood: "k", ActiveJob: c.job, BootID: "b"}
		if err := s1.store.saveDevice(&d, "hash-"+c.mac); err != nil {
			t.Fatal(err)
		}
		tr := &api.TrialResult{Before: own, After: c.after, Unchanged: c.same}
		if !c.same {
			tr.Changed = []string{"packages"}
		}
		if err := s1.store.saveJob(&api.Job{ID: c.job, Spec: api.JobSpec{Device: c.mac, Trial: true}, State: api.JobRestoring, Result: api.JobResult{Trial: tr}}); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := New(ctx, cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if h := s2.dev("m-changed").snapshot().TrialHold; !strings.Contains(h, "own system changed during Trial run j1102-000000-aaaa: packages") {
		t.Errorf("a changed run interrupted before its hold: hold %q", h)
	}
	if h := s2.dev("m-same").snapshot().TrialHold; h != "" {
		t.Errorf("an unchanged run interrupted after its check: hold %q", h)
	}
}

// A held Mac with a lease: the lease holder's own jobs are refused too, and a
// job the lease set aside before the hold is refused once the lease ends.
func TestAHeldMacWithALease(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	joshua := l.addMaintainer(harnessToken, "joshua")
	theirs := l.setAside(harnessToken, "ada@laptop", joshua.Token, "joshua@mini")
	l.s.dev(l.name).holdForTrial("own system changed during Trial run j0: packages")
	if code, _, body := l.submitAs(harnessToken, api.JobSpec{Kernel: l.artifact("krel=7.2-mine behavior=ok"), Holder: "ada@laptop"}); code != http.StatusConflict || !strings.Contains(body, "held after a Trial run") {
		t.Fatalf("the lease holder's job on a held Mac: %d %s", code, body)
	}
	if r, body := l.get("DELETE", "/api/devices/"+l.name+"/lease?holder=ada@laptop"); r.StatusCode != 200 {
		t.Fatalf("release: %s %s", r.Status, body)
	}
	got := l.jobAPI(theirs, 60*time.Second)
	if got.Outcome != api.OutcomeCanceled || !strings.Contains(got.Summary, "held after a Trial run") || got.Result.Booted {
		t.Fatalf("a set-aside job released on a held Mac: %s %q", got.Outcome, got.Summary)
	}
	l.baseline()
}
