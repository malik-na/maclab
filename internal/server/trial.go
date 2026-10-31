package server

// Testing any PR, and a Trial run.
//
// `lab run <mac> --pr <url|ref>` takes a pull request, branch or commit of
// any repository. labd pins it to a commit and looks at the tree: a Linux
// kernel tree is built and booted as a --kernel source is (the existing
// path), anything else is a userspace change. A userspace PR is a Trial
// run: its --script tests apply and test it in a Trial system, a Lab boot of
// the Known-good kernel from a writable btrfs snapshot of the Mac's own
// system, with the PR's tree (labd's git archive of the commit) unpacked in
// $MACLAB_PR_DIR. `--trial` alone runs scripts the same way with no PR.
//
// Restoring the Mac is the Lab boot's own return: the own system (@) is
// never mounted for the tests, so the reboot that ends every Lab boot (labd,
// the deadlines, the watchdog, the Availability window) boots it as it was.
// labd checks that: a read-only fingerprint of the own system (packages,
// pacman.conf, fstab, ESP files) before staging and after the restore must
// match, or the job fails and the Mac leaves the pool until a Maintainer
// looks.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iconidentify/maclab/internal/api"
)

// trialMinAgent knows Trial systems, own-system fingerprints and PR trees.
const trialMinAgent = "0.9.0"

// prArchiveMax caps a userspace PR tree's tar.
var prArchiveMax int64 = 1 << 30

// kernelTreeFiles are what make a tree a Linux kernel tree.
var kernelTreeFiles = []string{"Kconfig", "init/main.c", "arch/arm64/Kconfig"}

// validatePR checks a PR or Trial job at submit.
func validatePR(spec api.JobSpec, retryOf string) error {
	scripts := 0
	for _, t := range spec.Tests {
		if t.Script != "" {
			scripts++
		}
		if t.Trial || t.PR != nil {
			return fmt.Errorf("test %s: Trial and PR tests are labd's to set; use --trial or --pr", t.Name)
		}
	}
	if spec.PR != "" {
		if (spec.Kernel != "" || spec.Source != "") && retryOf == "" {
			return errors.New("give a PR or a kernel/source, not both")
		}
		if _, err := parseSource(spec.PR); err != nil {
			return err
		}
		switch spec.PRKind {
		case "", api.PRKernel:
		case api.PRUserspace:
			if scripts == 0 {
				return errors.New("a userspace PR needs a --script that applies and tests it (its tree is in $MACLAB_PR_DIR)")
			}
		default:
			return fmt.Errorf("pr_kind must be %s or %s", api.PRKernel, api.PRUserspace)
		}
	} else if spec.PRKind != "" {
		return errors.New("pr_kind needs a PR")
	}
	if spec.PR != "" || spec.Trial {
		if spec.Baseline || spec.Crash != "" || spec.InstalledManifest != "" || spec.Mesa != nil {
			return errors.New("a PR or Trial run can't also be a baseline, crash test, Installed run or Mesa job")
		}
	}
	if spec.Trial {
		if spec.PRKind == api.PRKernel {
			return errors.New("a kernel PR boots its kernel; it doesn't run in a Trial system")
		}
		if spec.Kernel != "" || spec.Source != "" {
			return errors.New("a Trial run boots the known-good kernel; drop --kernel")
		}
		if scripts == 0 {
			return errors.New("a Trial run needs at least one --script: it is what changes the Trial system")
		}
	}
	return nil
}

// trialAdmission is why the Mac can't take a Trial run (or a PR, which may
// become one), or nil.
func trialAdmission(d api.Device) error {
	switch {
	case !versionAtLeast(d.Facts.AgentVersion, trialMinAgent):
		return fmt.Errorf("%s runs lab-agent %s; PR and Trial runs need %s or later", d.Name, orStr(d.Facts.AgentVersion, "(unknown)"), trialMinAgent)
	case !d.SnapshotEligible:
		return fmt.Errorf("%s is not Eligible: its own system (@) can't be snapshotted, so it can't run a Trial system", d.Name)
	}
	return nil
}

// git runs git in dir with no prompts.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return out, fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out, nil
}

// fetchCommit makes a bare repository in a new directory under parent with
// src.SHA fetched (depth 1, filter as given), and returns it.
func fetchCommit(ctx context.Context, parent string, src api.Source, filter string) (string, error) {
	dir, err := os.MkdirTemp(parent, "pr-")
	if err != nil {
		return "", err
	}
	if _, err := git(ctx, dir, "init", "-q", "--bare"); err != nil {
		return dir, err
	}
	if _, err := git(ctx, dir, "remote", "add", "origin", src.Repo); err != nil {
		return dir, err
	}
	args := []string{"fetch", "-q", "--depth", "1"}
	if filter != "" {
		args = append(args, "--filter="+filter)
	}
	// by its ref when it has one (any server serves those); by sha if the ref moved
	want := src.SHA
	if src.Ref != "" && src.Ref != src.SHA {
		want = src.Ref
	}
	if _, err := git(ctx, dir, append(args, "origin", want)...); err == nil {
		if head, _ := git(ctx, dir, "rev-parse", "FETCH_HEAD"); strings.TrimSpace(string(head)) == src.SHA {
			return dir, nil
		}
	}
	if _, err := git(ctx, dir, append(args, "origin", src.SHA)...); err != nil {
		return dir, fmt.Errorf("fetching %s at %s: %w", src.Repo, src.SHA, err)
	}
	return dir, nil
}

// isKernelTree is whether the commit in repo has every kernelTreeFiles.
func isKernelTree(ctx context.Context, repo, sha string) (bool, error) {
	for _, f := range kernelTreeFiles {
		out, err := git(ctx, repo, "ls-tree", "--name-only", sha, "--", f)
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(string(out)) != f {
			return false, nil
		}
	}
	return true, nil
}

// resolvePR pins the job's PR to a commit, decides its kind, and sets the
// job up for that path: a kernel PR becomes the job's Source; a userspace
// PR makes it a Trial run whose scripts get the tree. It returns an outcome
// only on failure.
func (r *jobRun) resolvePR(ctx context.Context) (string, string) {
	j := r.j
	r.state(api.JobBuilding)
	r.ev("resolving PR %s", j.Spec.PR)
	src, err := parseSource(j.Spec.PR)
	if err == nil {
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		src, err = resolve(rctx, src)
		cancel()
	}
	if err != nil {
		return api.OutcomeBuildFailed, "PR: " + err.Error()
	}
	work := filepath.Join(r.s.cfg.DataDir, "pr-work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return api.OutcomeInfra, err.Error()
	}
	gctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res := api.PRResult{Source: src, Kind: j.Spec.PRKind}
	switch res.Kind {
	case api.PRKernel, api.PRUserspace:
		res.Why = "pr_kind given"
	default:
		// trees only: a kernel's blobs are the builder's to fetch
		repo, err := fetchCommit(gctx, work, src, "blob:none")
		defer os.RemoveAll(repo)
		if err != nil {
			return api.OutcomeBuildFailed, "PR: " + err.Error()
		}
		kernel, err := isKernelTree(gctx, repo, src.SHA)
		if err != nil {
			return api.OutcomeBuildFailed, "PR: " + err.Error()
		}
		res.Kind, res.Why = api.PRUserspace, "the tree has no "+strings.Join(kernelTreeFiles, ", ")
		if kernel {
			res.Kind, res.Why = api.PRKernel, "the tree has "+strings.Join(kernelTreeFiles, ", ")
		}
	}
	pinned := src.Repo + "#" + src.SHA
	r.ev("PR %s is %s at %s: %s (%s)", j.Spec.PR, shortRepo(src.Repo), src.SHA[:12], res.Kind, res.Why)
	if res.Kind == api.PRKernel {
		r.s.updateJob(j, func(j *api.Job) {
			j.Spec.PRKind, j.Spec.Source, j.Result.PR = api.PRKernel, pinned, &res
		})
		return "", ""
	}
	scripts := 0
	for _, t := range j.Spec.Tests {
		if t.Script != "" {
			scripts++
		}
	}
	if scripts == 0 {
		r.s.updateJob(j, func(j *api.Job) { j.Result.PR = &res })
		return api.OutcomeInfra, "a userspace PR needs a --script that applies and tests it (its tree is in $MACLAB_PR_DIR)"
	}
	if d := r.v.snapshot(); trialAdmission(d) != nil {
		r.s.updateJob(j, func(j *api.Job) { j.Result.PR = &res })
		return api.OutcomeInfra, "a userspace PR runs in a Trial system: " + trialAdmission(d).Error()
	}
	repo, err := fetchCommit(gctx, work, src, "")
	defer os.RemoveAll(repo)
	if err != nil {
		return api.OutcomeBuildFailed, "PR: " + err.Error()
	}
	sha, n, err := r.s.archiveCommit(gctx, repo, src.SHA)
	if err != nil {
		return api.OutcomeBuildFailed, "PR tree: " + err.Error()
	}
	res.Archive = sha
	r.ev("PR tree %s: %s (%d KB), for the Trial run's scripts in $MACLAB_PR_DIR", src.SHA[:12], short(sha), n>>10)
	r.s.updateJob(j, func(j *api.Job) {
		j.Spec.PRKind, j.Spec.PR, j.Spec.Trial, j.Result.PR = api.PRUserspace, pinned, true, &res
	})
	return "", ""
}

// archiveCommit stores git archive's tar of sha as an artifact.
func (s *Server) archiveCommit(ctx context.Context, repo, sha string) (string, int64, error) {
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", sha)
	cmd.Dir = repo
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", 0, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", 0, err
	}
	lr := &io.LimitedReader{R: out, N: prArchiveMax + 1}
	art, n, serr := s.storeArtifact(lr)
	if serr != nil {
		io.Copy(io.Discard, out)
	}
	werr := cmd.Wait()
	switch {
	case serr != nil:
		return "", 0, serr
	case werr != nil:
		return "", 0, fmt.Errorf("git archive: %v: %s", werr, strings.TrimSpace(stderr.String()))
	case n > prArchiveMax:
		os.Remove(s.artifactPath(art))
		return "", 0, fmt.Errorf("the tree is more than %d bytes", prArchiveMax)
	}
	return art, n, nil
}

// prTree is the tree a Trial run's scripts get, nil without a userspace PR.
func (r *jobRun) prTree() *api.PRTree {
	pr := r.j.Result.PR
	if pr == nil || pr.Kind != api.PRUserspace || pr.Archive == "" {
		return nil
	}
	return &api.PRTree{Repo: pr.Source.Repo, Ref: pr.Source.Ref, Commit: pr.Source.SHA, Archive: pr.Archive}
}

// ownSystem reads the Mac's own system's fingerprint.
func (r *jobRun) ownSystem(ctx context.Context) (*api.OwnSystem, error) {
	var o api.OwnSystem
	if err := r.v.call(ctx, api.CmdOwnSystem, r.j.ID, nil, 2*time.Minute, &o); err != nil {
		return nil, err
	}
	if o.Root == "" || len(o.Digests) == 0 {
		return nil, errors.New("lab-agent returned an empty fingerprint")
	}
	return &o, nil
}

// trialBefore records the own system before a Trial run touches anything.
func (r *jobRun) trialBefore(ctx context.Context) (string, string) {
	o, err := r.ownSystem(ctx)
	if err != nil {
		return api.OutcomeInfra, "the own system's fingerprint before the Trial run: " + err.Error()
	}
	r.s.updateJob(r.j, func(j *api.Job) {
		j.Result.Trial = &api.TrialResult{Subvolume: "/maclab-trial/" + j.ID + "/@", Before: o}
	})
	r.ev("own system before the Trial run: root %s, %d fingerprints", o.Root, len(o.Digests))
	return "", ""
}

// trialAfter checks, once the Mac is back on its own system, that the Trial
// run left it as it was. A Mac whose own system changed leaves the pool.
func (r *jobRun) trialAfter(ctx context.Context, outcome, summary string) (string, string) {
	tr := r.j.Result.Trial
	if tr == nil || tr.Before == nil {
		return outcome, summary
	}
	o, err := r.ownSystem(ctx)
	if err != nil {
		r.ev("own system after the Trial run: %v", err)
		if outcome == api.OutcomePass {
			outcome = api.OutcomeInfra
		}
		return outcome, summary + "; the own system could not be checked after the Trial run: " + err.Error()
	}
	changed := tr.Before.Diff(*o)
	r.s.updateJob(r.j, func(j *api.Job) {
		j.Result.Trial.After, j.Result.Trial.Changed, j.Result.Trial.Unchanged = o, changed, len(changed) == 0
	})
	if len(changed) == 0 {
		r.ev("own system after the Trial run: unchanged (%d fingerprints match)", len(o.Digests))
		return outcome, summary + "; own system unchanged"
	}
	why := "own system changed during Trial run " + r.j.ID + ": " + strings.Join(changed, ", ")
	r.ev("%s", why)
	r.v.setState(api.StateNew, why+"; a Maintainer must look before it runs again")
	return api.OutcomeInfra, summary + "; " + why + ". The Mac is out of the pool"
}
