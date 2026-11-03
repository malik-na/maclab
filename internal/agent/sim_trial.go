package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/iconidentify/maclab/internal/api"
)

// simOwnPkgs is the simulated own system's package list until a test sets one.
var simOwnPkgs = []string{"linux-asahi 7.1.12-good", "omarchy 4.5", "pacman 7.0"}

// stageTrial stages a Lab boot of the known-good kernel from a new simulated
// Trial system: a copy of the own system's packages.
func (s *Sim) stageTrial(job string, a api.StageArgs) (api.StageResult, error) {
	if a.Artifact != "" || a.Installed != nil {
		return api.StageResult{}, errors.New("a Trial run boots the known-good kernel; it takes no kernel artifact or installed entry")
	}
	if err := s.SnapshotEligible(); err != nil {
		return api.StageResult{}, err
	}
	// the simulated own system is @, though its cmdline doesn't say so
	cmdline, err := stageCmdline(s.cmdline+" rootflags=subvol=/@", func() (string, error) { return "root=UUID=sim rw quiet splash", nil }, a, job)
	if err != nil {
		return api.StageResult{}, err
	}
	if cmdline, err = trialCmdline(cmdline, job); err != nil {
		return api.StageResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.trials[job]; ok {
		return api.StageResult{}, fmt.Errorf("a Trial system for %s is already there", job)
	}
	if s.trials == nil {
		s.trials = map[string][]string{}
	}
	s.trials[job] = slices.Clone(s.ownPkgsLocked())
	s.staged[job] = simEntry{krel: SimKnownGood, behavior: "ok", cmdline: cmdline, trial: job}
	return api.StageResult{KernelRelease: SimKnownGood, Entry: entryPrefix + job, Cmdline: cmdline}, nil
}

func (s *Sim) ownPkgsLocked() []string {
	if s.ownPkgs == nil {
		s.ownPkgs = slices.Clone(simOwnPkgs)
	}
	return s.ownPkgs
}

// trialTest is what a test does to the simulated disks: a Maintainer's
// script in a Trial system installs a package there (and, with trialLeak,
// in the own system too); its PR tree is fetched first.
func (s *Sim) trialTest(ctx context.Context, job string, t api.TestSpec, fetch Fetcher) error {
	if t.PR != nil {
		p := filepath.Join(s.WorkDir, job+"-pr.tar")
		if err := fetch(ctx, t.PR.Archive, p); err != nil {
			return fmt.Errorf("PR tree %s: %w", t.PR.Archive, err)
		}
		s.mu.Lock()
		s.prTrees = append(s.prTrees, t.PR.Commit)
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.Script == "" || s.trial == "" {
		return nil
	}
	pkg := "trial-" + t.Name + " 1.0"
	s.trials[s.trial] = append(s.trials[s.trial], pkg)
	if s.trialLeak {
		s.ownPkgs = append(slices.Clone(s.ownPkgsLocked()), pkg)
	}
	return nil
}

// RunningTrial is the job whose simulated Trial system this boot runs from.
func (s *Sim) RunningTrial() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trial
}

// OwnSystem fingerprints the simulated own system: its packages and the kernel a plain boot boots.
func (s *Sim) OwnSystem() (api.OwnSystem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if IsLabBoot(s.cmdline) {
		return api.OwnSystem{}, errors.New("this is a Lab boot, not the Mac's own system")
	}
	def := s.DefaultKernel
	if def == "" {
		def = SimKnownGood
	}
	return api.OwnSystem{Root: "/@", Digests: map[string]string{
		"packages":       sum([]byte(strings.Join(s.ownPkgsLocked(), "\n"))),
		"default-kernel": sum([]byte(def)),
	}}, nil
}

// SetTrialLeak makes a Trial test change the own system too, as broken isolation would.
func (s *Sim) SetTrialLeak(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trialLeak = on
}

// Trials lists the simulated Trial systems still on disk, by job, sorted.
func (s *Sim) Trials() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.trials))
}

// TrialPackages is a simulated Trial system's package list.
func (s *Sim) TrialPackages(job string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.trials[job])
}

// OwnPackages is the simulated own system's package list.
func (s *Sim) OwnPackages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ownPkgsLocked())
}

// PRTrees lists the PR commits Trial tests fetched, in order.
func (s *Sim) PRTrees() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.prTrees)
}
