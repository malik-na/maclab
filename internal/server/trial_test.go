package server

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iconidentify/maclab/internal/agent"
	"github.com/iconidentify/maclab/internal/api"
)

// trialScript is a Maintainer's script for a Trial run; the simulated Mac
// installs a package in the Trial system when it runs one.
func (l *lab) trialScript(name string) api.TestSpec {
	return api.TestSpec{Name: name, Script: l.artifact("#!/bin/sh\npacman -S --noconfirm something\n")}
}

func (l *lab) noTrialsLeft() {
	l.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(l.sim.Trials()) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := l.sim.Trials(); len(got) > 0 {
		l.t.Errorf("Trial systems left on the Mac: %v", got)
	}
}

func TestTrialRunLeavesTheOwnSystemAsItWas(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	own := l.sim.OwnPackages()
	j := l.run(api.JobSpec{Trial: true, Tests: []api.TestSpec{l.trialScript("migrate")}})
	l.expect(j, api.OutcomePass)

	if !strings.Contains(j.Result.BootCmdline, "rootflags=subvol=/maclab-trial/"+j.ID+"/@") {
		t.Errorf("the Lab boot did not run from its Trial system: %s", j.Result.BootCmdline)
	}
	tr := j.Result.Trial
	if tr == nil || tr.Before == nil || tr.After == nil || !tr.Unchanged || len(tr.Changed) > 0 {
		t.Fatalf("Trial result %+v", tr)
	}
	if !strings.Contains(j.Summary, "own system unchanged") {
		t.Errorf("summary %q", j.Summary)
	}
	if got := l.sim.OwnPackages(); !slices.Equal(got, own) {
		t.Errorf("own packages %v, were %v", got, own)
	}
	l.noTrialsLeft()
	d := l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })
	if d.Kernel != agent.SimKnownGood || d.Health.JobTag != "" {
		t.Errorf("back on %s (lab entry %q)", d.Kernel, d.Health.JobTag)
	}
}

func TestTrialRunThatChangesTheOwnSystemFailsAndLeavesThePool(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	l.sim.SetTrialLeak(true) // isolation broken: the script's change reaches the own system
	j := l.run(api.JobSpec{Trial: true, Tests: []api.TestSpec{l.trialScript("migrate")}})
	l.expect(j, api.OutcomeInfra)
	if tr := j.Result.Trial; tr == nil || tr.Unchanged || !slices.Equal(tr.Changed, []string{"packages"}) {
		t.Fatalf("Trial result %+v", tr)
	}
	if !strings.Contains(j.Summary, "own system changed") || !strings.Contains(j.Summary, "packages") {
		t.Errorf("summary %q", j.Summary)
	}
	if d := l.s.dev(l.name).snapshot(); d.State != api.StateNew {
		t.Errorf("a Mac whose own system changed is %s, not out of the pool", d.State)
	}
}

func TestTrialRunThatHangsComesBackToTheOwnSystem(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	l.sim.HangFor = 500 * time.Millisecond
	own := l.sim.OwnPackages()
	j := l.run(api.JobSpec{Trial: true, Tests: []api.TestSpec{l.trialScript("hang-migrate")}})
	if j.Outcome == api.OutcomePass {
		t.Fatalf("a hung Trial run passed: %s", j.Summary)
	}
	if tr := j.Result.Trial; tr == nil || !tr.Unchanged {
		t.Fatalf("after a hang the own system was not checked unchanged: %+v (%s)", tr, j.Summary)
	}
	if got := l.sim.OwnPackages(); !slices.Equal(got, own) {
		t.Errorf("own packages %v, were %v", got, own)
	}
	l.noTrialsLeft()
	l.waitDevice(func(d api.Device) bool { return d.Kernel == agent.SimKnownGood && d.Health.JobTag == "" })
}

// A Trial run whose restore fails is still checked when the Mac is back on
// its own system anyway: here only the cleanup failed, and the own system
// changed, so the Mac leaves the pool rather than going back to it.
func TestTrialRunWhoseRestoreFailsIsStillChecked(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	l.sim.SetTrialLeak(true)
	l.sim.FailCleanup(func(job string, all bool) error {
		if all {
			return nil
		}
		return errors.New("btrfs subvolume delete: device busy")
	})
	j := l.run(api.JobSpec{Trial: true, Tests: []api.TestSpec{l.trialScript("migrate")}})
	l.expect(j, api.OutcomeInfra)
	if !strings.Contains(j.Summary, "restore failed") || !strings.Contains(j.Summary, "own system changed") {
		t.Errorf("summary %q", j.Summary)
	}
	if tr := j.Result.Trial; tr == nil || tr.Unchanged || !slices.Equal(tr.Changed, []string{"packages"}) {
		t.Fatalf("Trial result %+v", tr)
	}
	if d := l.s.dev(l.name).snapshot(); d.State != api.StateNew {
		t.Errorf("a Mac whose own system changed is %s, not out of the pool", d.State)
	}
}

// A Trial run whose own system can't be checked afterwards can't say it is
// as it was: the Mac leaves the pool and stays out, through needing hands
// and coming back and through a job ending, until a passing baseline.
func TestTrialRunNotCheckedAfterwardsStaysOutOfThePool(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	v := l.s.dev(l.name)
	v.mu.Lock()
	v.setStateLocked(api.StateNeedsHands, "power cycle it")
	v.mu.Unlock()
	reason := v.markTrialUnchecked("j1", "it was not back on it")
	if d := v.snapshot(); d.State != api.StateNeedsHands || d.TrialUnchecked != reason {
		t.Fatalf("a Mac that needs hands is %s (mark %q)", d.State, d.TrialUnchecked)
	}
	// its next check-in brings it back, as from any power cycle
	d := l.waitDevice(func(d api.Device) bool { return d.State != api.StateNeedsHands })
	if d.State != api.StateNew || !strings.Contains(d.StateReason, "not checked after Trial run j1") {
		t.Fatalf("an unchecked Mac came back %s (%q)", d.State, d.StateReason)
	}
	// a job ending, as every job's does, can't put it back either
	v.mu.Lock()
	v.setStateLocked(v.idleStateLocked(), "")
	v.mu.Unlock()
	if d := v.snapshot(); d.State != api.StateNew {
		t.Fatalf("after a job an unchecked Mac is %s", d.State)
	}
	l.baseline()
	if d := l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady }); d.TrialUnchecked != "" {
		t.Errorf("a passing baseline left the mark: %q", d.TrialUnchecked)
	}
}

// labd restarting mid Trial run ends the job with no after-check: the Mac is
// marked unchecked, in the stored device, and does not come back ready,
// through another restart too.
func TestLabdRestartMidTrialRunLeavesTheMacUnchecked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{DataDir: t.TempDir(), FirstMaintainer: harnessMaintainer, FirstMaintainerToken: "a"}
	quiet := log.New(io.Discard, "", 0)
	s1, err := New(ctx, cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	const job = "j1102-000000-abcd"
	d := api.Device{ID: "d1", Name: "m1", State: api.StateBusy, KnownGood: "k", ActiveJob: job, BootID: "b"}
	if err := s1.store.saveDevice(&d, "hash"); err != nil {
		t.Fatal(err)
	}
	j := &api.Job{ID: job, Spec: api.JobSpec{Device: "m1", Trial: true}, State: api.JobTesting,
		Result: api.JobResult{Trial: &api.TrialResult{Before: &api.OwnSystem{Root: "/@", Digests: map[string]string{"packages": "x"}}}}}
	if err := s1.store.saveJob(j); err != nil {
		t.Fatal(err)
	}
	// labd restarts while the Mac runs the Trial system's tests
	s2, err := New(ctx, cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s2.store.job(job); err != nil || got.State != api.JobDone || !strings.Contains(got.Summary, "not checked after Trial run") {
		t.Fatalf("job after restart: %+v %v", got, err)
	}
	v := s2.dev("m1")
	if !strings.Contains(v.snapshot().TrialUnchecked, job) {
		t.Fatalf("no unchecked mark after the restart: %+v", v.snapshot())
	}
	// it checks in again: not ready
	v.mu.Lock()
	v.setStateLocked(api.StateReady, "")
	v.mu.Unlock()
	if st := v.snapshot().State; st != api.StateNew {
		t.Fatalf("an unchecked Mac came back %s", st)
	}
	s3, err := New(ctx, cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if m := s3.dev("m1").snapshot().TrialUnchecked; !strings.Contains(m, job) {
		t.Errorf("the mark did not survive another restart: %q", m)
	}
}

func TestTrialAndPRRefusals(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	script := l.trialScript("s")
	kernel := l.artifact("krel=7.2-x behavior=ok")
	for name, spec := range map[string]api.JobSpec{
		"trial without a script":      {Trial: true},
		"trial with a kernel":         {Trial: true, Kernel: kernel, Tests: []api.TestSpec{script}},
		"trial baseline":              {Trial: true, Baseline: true, Tests: []api.TestSpec{script}},
		"pr and kernel":               {PR: "https://github.com/o/r/pull/1", Kernel: kernel},
		"userspace pr without script": {PR: "https://github.com/o/r/pull/1", PRKind: api.PRUserspace},
		"bad pr kind":                 {PR: "https://github.com/o/r/pull/1", PRKind: "firmware"},
		"pr kind without pr":          {PRKind: api.PRKernel},
		"kernel pr in a trial":        {PR: "https://github.com/o/r/pull/1", PRKind: api.PRKernel, Trial: true, Tests: []api.TestSpec{script}},
		"a test claiming trial":       {Tests: []api.TestSpec{{Name: "s", Script: script.Script, Trial: true}}},
	} {
		spec.Device = l.name
		if _, code, err := l.s.Submit(spec); err == nil || code != 400 {
			t.Errorf("%s: %d %v", name, code, err)
		}
	}
	l.sim.SetSnapshotable(false)
	l.waitDevice(func(d api.Device) bool { return !d.SnapshotEligible })
	if _, code, err := l.s.Submit(api.JobSpec{Device: l.name, Trial: true, Tests: []api.TestSpec{script}}); code != 409 || err == nil || !strings.Contains(err.Error(), "Eligible") {
		t.Errorf("a Mac that can't be snapshotted took a Trial run: %d %v", code, err)
	}
}

func TestTrialTestIsRefusedOnTheOwnSystem(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	t1 := l.trialScript("migrate")
	t1.Trial = true
	err := l.s.dev(l.name).call(context.Background(), api.CmdRunTest, "j1031-000000-ab12", t1, 30*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "own system") {
		t.Fatalf("lab-agent ran a Trial test on the Mac's own system: %v", err)
	}
	if got := l.sim.OwnPackages(); slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, "trial-") }) {
		t.Errorf("own packages %v", got)
	}
}

// gitRepoWith is a repository with these files on main, as file://dir@main.
func gitRepoWith(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	run("add", ".")
	run("commit", "-q", "-m", "x")
	return "file://" + dir + "@main"
}

func TestUserspacePRRunsInATrialSystem(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	pr := gitRepoWith(t, map[string]string{"bin/omarchy-mac-migrate": "#!/bin/sh\necho migrate\n", "README.md": "x\n"})
	j := l.run(api.JobSpec{PR: pr, Tests: []api.TestSpec{l.trialScript("apply-pr")}})
	l.expect(j, api.OutcomePass)
	res := j.Result.PR
	if res == nil || res.Kind != api.PRUserspace || len(res.Source.SHA) != 40 || res.Archive == "" {
		t.Fatalf("PR result %+v", res)
	}
	if !j.Spec.Trial || j.Spec.PRKind != api.PRUserspace || !strings.HasSuffix(j.Spec.PR, "#"+res.Source.SHA) {
		t.Errorf("the job was not pinned as a userspace Trial run: %+v", j.Spec)
	}
	if got := l.sim.PRTrees(); !slices.Equal(got, []string{res.Source.SHA}) {
		t.Errorf("the Trial script fetched PR trees %v, want %s", got, res.Source.SHA)
	}
	// the archive is the commit's tree
	f, err := os.Open(l.s.artifactPath(res.Archive))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var names []string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if !slices.Contains(names, "bin/omarchy-mac-migrate") || !slices.Contains(names, "README.md") {
		t.Errorf("archive holds %v", names)
	}
	if tr := j.Result.Trial; tr == nil || !tr.Unchanged {
		t.Errorf("Trial result %+v", tr)
	}
	l.noTrialsLeft()
}

func TestUserspacePRWithoutAScriptIsRefusedBeforeTheMac(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	boots := l.sim.Boots
	j := l.run(api.JobSpec{PR: gitRepoWith(t, map[string]string{"README.md": "x\n"})})
	l.expect(j, api.OutcomeInfra)
	if j.Result.PR == nil || j.Result.PR.Kind != api.PRUserspace || !strings.Contains(j.Summary, "--script") {
		t.Errorf("%+v %s", j.Result.PR, j.Summary)
	}
	if l.sim.Boots != boots {
		t.Error("the Mac rebooted for a PR that had nothing to run")
	}
}

func TestKernelPRIsBuiltAndBooted(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	builds := 0
	go l.fakeBuilder(ctx, &builds)
	pr := gitRepoWith(t, map[string]string{"Kconfig": "x\n", "Makefile": "all:\n", "init/main.c": "int main;\n", "arch/arm64/Kconfig": "x\n"})
	j := l.run(api.JobSpec{PR: pr})
	l.expect(j, api.OutcomePass)
	res := j.Result.PR
	if res == nil || res.Kind != api.PRKernel || res.Archive != "" {
		t.Fatalf("PR result %+v", res)
	}
	if j.Spec.Trial || j.Result.Trial != nil || j.Spec.Build == "" || !strings.HasSuffix(j.Spec.Source, "#"+res.Source.SHA) {
		t.Errorf("a kernel PR did not go the source-build path: %+v", j.Spec)
	}
	if !strings.HasPrefix(j.Result.BootKernel, "7.2-src") || builds != 1 {
		t.Errorf("booted %q after %d builds", j.Result.BootKernel, builds)
	}
}

func TestPRKindOverridesTheTree(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	// a kernel-shaped tree, tested as userspace (say, a script in a kernel repo)
	pr := gitRepoWith(t, map[string]string{"Kconfig": "x\n", "init/main.c": "x\n", "arch/arm64/Kconfig": "x\n", "tools/t.sh": "x\n"})
	j := l.run(api.JobSpec{PR: pr, PRKind: api.PRUserspace, Tests: []api.TestSpec{l.trialScript("t")}})
	l.expect(j, api.OutcomePass)
	if j.Result.PR == nil || j.Result.PR.Kind != api.PRUserspace || j.Result.PR.Why != "pr_kind given" || !j.Spec.Trial {
		t.Errorf("%+v", j.Result.PR)
	}
}

func TestArchiveCommitIsCapped(t *testing.T) {
	l := newLab(t, false)
	old := prArchiveMax
	prArchiveMax = 10
	defer func() { prArchiveMax = old }()
	repo := strings.TrimSuffix(strings.TrimPrefix(gitRepoWith(t, map[string]string{"big": strings.Repeat("x", 4096)}), "file://"), "@main")
	head, err := git(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.s.archiveCommit(context.Background(), repo, string(bytes.TrimSpace(head))); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("an oversized tree: %v", err)
	}
}
