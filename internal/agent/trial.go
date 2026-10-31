package agent

// A Trial system is a writable btrfs snapshot of the Mac's own system (@)
// that one job's Lab boot runs from, so a test can change packages,
// pacman.conf, /etc and the boot setup without touching the Mac's own
// system. Nothing is rolled back: the own system is never written, and the
// Lab boot's end (a reboot, whatever ends it) boots it again, as every Lab
// boot does. Cleanup deletes the snapshot afterwards.
//
// Layout, on the root filesystem's top level (subvolid=5):
//
//	maclab-trial/<job>/@        the Trial system's /, a snapshot of @
//	maclab-trial/<job>/<sub>    a snapshot of each other subvolume fstab
//	                            mounts from it (@home, @pkg, ...), except
//	                            /var/log, which stays shared so the Lab
//	                            boot's journal outlives it
//
// The Trial system's fstab is rewritten so that nothing of the own system is
// mounted read-write but lab-agent's own state: the ESP is not mounted (the
// Trial system gets a copy of its files at the same path instead), swap is
// off, other disks are left out, and /var/lib/maclab is bind-mounted from
// the own system, so the Audit log, the Availability window, the Access
// level and the Lab boot's deadline record stay the Mac's one copy.
//
// lab-agent keeps a record per Trial system under its work directory
// (trials/<job>), so cleanup mounts the top level only when there is one.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/iconidentify/maclab/internal/api"
)

const (
	trialTopDir     = "maclab-trial"           // on the top level of the root filesystem
	trialRecordDir  = "trials"                 // under lab-agent's work directory
	trialTopMount   = "/run/maclab/trial-top"  // where lab-agent mounts the top level
	trialOwnMount   = "/run/maclab/own-system" // where a Trial system mounts @, for the bind
	trialMarker     = "etc/maclab-trial"       // in the Trial system: the job it belongs to
	trialOwnFstab   = "etc/fstab.maclab-own"   // in the Trial system: the fstab it was copied with
	trialESPCopyMax = int64(2) << 30           // the ESP copy's cap
	trialStateDir   = "/var/lib/maclab"        // lab-agent's state, shared with the own system
	trialLogTarget  = "/var/log"               // shared: the Lab boot's journal outlives it
)

var reJobName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// trialRoot is the btrfs subvolume path of a job's Trial system root.
func trialRoot(job string) string { return "/" + trialTopDir + "/" + job + "/@" }

// reTrialRoot matches a Trial system's root subvolume, as findmnt shows it.
var reTrialRoot = regexp.MustCompile(`^/` + trialTopDir + `/([A-Za-z0-9][A-Za-z0-9_-]*)/@$`)

// trialOfSubvol is the job whose Trial system root sub is, or "".
func trialOfSubvol(sub string) string {
	if m := reTrialRoot.FindStringSubmatch(sub); m != nil {
		return m[1]
	}
	return ""
}

// trialCmdline turns the Lab boot cmdline into the Trial system's: rootflags'
// subvol=@ becomes the job's Trial root, a resume from the own system's swap
// is dropped, and systemd.gpt_auto=0 keeps systemd-gpt-auto-generator from
// mounting the real ESP (Limine sets LoaderDevicePartUUID, and the Trial
// fstab has no /boot or /boot/efi entry, so it otherwise would). It refuses
// a cmdline that doesn't name @ explicitly, since then it can't be sure the
// boot mounts the Trial system and not @.
func trialCmdline(cmdline, job string) (string, error) {
	if !reJobName.MatchString(job) {
		return "", fmt.Errorf("bad job id %q", job)
	}
	var out []string
	found := 0
	for _, f := range strings.Fields(cmdline) {
		name, val, _ := strings.Cut(f, "=")
		switch name {
		case "resume", "resume_offset", "hibernate", "systemd.gpt_auto", "rd.systemd.gpt_auto":
			continue
		case "rootflags":
			opts := strings.Split(val, ",")
			for i, o := range opts {
				k, v, _ := strings.Cut(o, "=")
				switch k {
				case "subvolid":
					return "", errors.New("rootflags names the root by subvolid; a Trial run needs subvol=@")
				case "subvol":
					if v != "@" && v != "/@" {
						return "", fmt.Errorf("rootflags mounts subvol=%s, not @", v)
					}
					opts[i] = "subvol=" + trialRoot(job)
					found++
				}
			}
			f = "rootflags=" + strings.Join(opts, ",")
		}
		out = append(out, f)
	}
	if found != 1 {
		return "", errors.New("the known-good cmdline has no single rootflags=subvol=@, so a Trial system's boot could mount the own system")
	}
	out = append(out, "systemd.gpt_auto=0")
	return strings.Join(out, " "), nil
}

// fstabLine is one entry of /etc/fstab.
type fstabLine struct {
	spec, target, fstype, opts string
	rest                       []string
}

func parseFstabLine(line string) (fstabLine, bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return fstabLine{}, false
	}
	f := strings.Fields(t)
	if len(f) < 4 {
		return fstabLine{}, false
	}
	return fstabLine{spec: f[0], target: f[1], fstype: f[2], opts: f[3], rest: f[4:]}, true
}

func (e fstabLine) String() string {
	return strings.Join(append([]string{e.spec, e.target, e.fstype, e.opts}, e.rest...), " ")
}

// subvol is the subvol= option, "" if none.
func optSubvol(opts string) string {
	for _, o := range strings.Split(opts, ",") {
		if v, ok := strings.CutPrefix(o, "subvol="); ok {
			return v
		}
	}
	return ""
}

func setSubvol(opts, sub string) string {
	parts := strings.Split(opts, ",")
	for i, o := range parts {
		if strings.HasPrefix(o, "subvol=") {
			parts[i] = "subvol=" + sub
		}
	}
	return strings.Join(parts, ",")
}

// dropOpt removes every option named name (name or name=...) from opts.
func dropOpt(opts, name string) string {
	parts := slices.DeleteFunc(strings.Split(opts, ","), func(o string) bool {
		k, _, _ := strings.Cut(o, "=")
		return k == name
	})
	return strings.Join(parts, ",")
}

// pseudoFS are filesystems with nothing on disk, which a Trial system mounts as the own system does.
var pseudoFS = map[string]bool{"tmpfs": true, "proc": true, "sysfs": true, "devpts": true, "efivarfs": true, "ramfs": true,
	"cgroup2": true, "bpf": true, "debugfs": true, "securityfs": true, "configfs": true, "tracefs": true, "hugetlbfs": true, "mqueue": true}

// trialPlan is what preparing a Trial system does.
type trialPlan struct {
	Fstab     string   // the Trial system's /etc/fstab
	Subvols   []string // top-level subvolumes to snapshot, "@" first
	ESPTarget string   // where the ESP is mounted, whose files the Trial system gets a copy of
}

// planTrial rewrites the own system's fstab for a job's Trial system. espTarget
// is where the ESP is mounted ("" for none). It refuses what it can't place:
// a / that isn't subvol=@, a btrfs mount of the root filesystem without a
// subvol= (the top level), or a nested subvolume path.
func planTrial(fstab, espTarget, job string) (trialPlan, error) {
	p := trialPlan{ESPTarget: espTarget}
	if !reJobName.MatchString(job) {
		return p, fmt.Errorf("bad job id %q", job)
	}
	var rootSpec, rootOpts string
	for _, line := range strings.Split(fstab, "\n") {
		if e, ok := parseFstabLine(line); ok && e.target == "/" {
			if rootSpec != "" {
				return p, errors.New("fstab mounts / twice")
			}
			rootSpec, rootOpts = e.spec, e.opts
		}
	}
	if rootSpec == "" {
		return p, errors.New("fstab has no / entry")
	}
	if s := strings.TrimPrefix(optSubvol(rootOpts), "/"); s != "@" {
		return p, fmt.Errorf("fstab mounts / from subvol %q, not @", optSubvol(rootOpts))
	}
	p.Subvols = []string{"@"}
	var out []string
	off := func(line, why string) { out = append(out, "# maclab Trial system: "+why+": "+line) }
	for _, line := range strings.Split(strings.TrimRight(fstab, "\n"), "\n") {
		e, ok := parseFstabLine(line)
		if !ok {
			out = append(out, line)
			continue
		}
		switch {
		case e.target == "/":
			e.opts = setSubvol(e.opts, trialRoot(job))
			out = append(out, e.String())
		case e.fstype == "swap" || e.target == "none" || e.target == "swap":
			off(line, "no swap")
		case espTarget != "" && e.target == espTarget:
			off(line, "the ESP is not mounted; this is a copy of its files")
		case e.spec == rootSpec && e.fstype == "btrfs":
			sub := strings.TrimPrefix(optSubvol(e.opts), "/")
			switch {
			case sub == "":
				return p, fmt.Errorf("fstab mounts the root filesystem at %s without a subvol=", e.target)
			case strings.Contains(sub, "/"):
				return p, fmt.Errorf("fstab mounts nested subvolume %s at %s", sub, e.target)
			case sub == "@":
				return p, fmt.Errorf("fstab mounts @ again at %s", e.target)
			case e.target == trialLogTarget:
				out = append(out, line) // shared on purpose: the journal
			default:
				p.Subvols = append(p.Subvols, sub)
				e.opts = setSubvol(e.opts, "/"+trialTopDir+"/"+job+"/"+sub)
				out = append(out, e.String())
			}
		case pseudoFS[e.fstype]:
			out = append(out, line)
		default:
			off(line, "not mounted in a Trial system")
		}
	}
	// x-systemd.growfs would have systemd grow the own system's filesystem
	// from a mount that is only there for the bind
	ownOpts := dropOpt(setSubvol(rootOpts, "/@"), "x-systemd.growfs")
	out = append(out,
		"# maclab Trial system: lab-agent's state is the own system's, so the Audit log, window and deadlines stay one copy",
		fstabLine{spec: rootSpec, target: trialOwnMount, fstype: "btrfs", opts: ownOpts, rest: []string{"0", "0"}}.String(),
		fstabLine{spec: trialOwnMount + trialStateDir, target: trialStateDir, fstype: "none", opts: "bind,x-systemd.requires-mounts-for=" + trialOwnMount, rest: []string{"0", "0"}}.String())
	p.Fstab = strings.Join(out, "\n") + "\n"
	return p, nil
}

// btrfsDevice is the device of a findmnt SOURCE such as /dev/nvme0n1p5[/@].
func btrfsDevice(source string) string {
	if i := strings.Index(source, "["); i > 0 {
		return source[:i]
	}
	return source
}

// rootSubvol is the btrfs subvolume / is mounted from ("" if none).
func rootSubvol(root mount) string {
	if s := reSubvol.FindStringSubmatch(root.Source); s != nil {
		return s[1]
	}
	return ""
}

// withTop mounts the root filesystem's top level for fn.
func withTop(fn func(top string) error) error {
	root, err := findMount("/")
	if err != nil {
		return err
	}
	if root.FSType != "btrfs" {
		return fmt.Errorf("/ is %s, not btrfs", root.FSType)
	}
	if err := os.MkdirAll(trialTopMount, 0o700); err != nil {
		return err
	}
	if err := unix.Mount(btrfsDevice(root.Source), trialTopMount, "btrfs", unix.MS_NOATIME, "subvolid=5"); err != nil {
		return fmt.Errorf("mounting the top level of %s: %w", btrfsDevice(root.Source), err)
	}
	defer unix.Unmount(trialTopMount, 0)
	return fn(trialTopMount)
}

func btrfs(args ...string) error {
	if out, err := exec.Command("btrfs", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("btrfs %s: %v: %s", strings.Join(args, " "), err, lastLines(string(out), 5))
	}
	return nil
}

func (l *Linux) trialRecord(job string) string {
	return filepath.Join(l.WorkDir, trialRecordDir, job)
}

// prepareTrial makes job's Trial system. On any error it removes what it made.
func (l *Linux) prepareTrial(job string) (err error) {
	root, err := findMount("/")
	if err != nil {
		return err
	}
	if root.FSType != "btrfs" || rootSubvol(root) != "/@" {
		return fmt.Errorf("a Trial run needs / on the btrfs subvolume @ (it is %s %q)", root.FSType, rootSubvol(root))
	}
	esp, err := findESP()
	if err != nil {
		return err
	}
	fstab, err := os.ReadFile("/etc/fstab")
	if err != nil {
		return err
	}
	plan, err := planTrial(string(fstab), esp.Target, job)
	if err != nil {
		return fmt.Errorf("can't place a Trial system: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(l.trialRecord(job)), 0o700); err != nil {
		return err
	}
	// the record first: whatever happens next, cleanup knows to look
	if err := os.WriteFile(l.trialRecord(job), []byte(trialRoot(job)+"\n"), 0o600); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			l.removeTrials(job, false)
		}
	}()
	return withTop(func(top string) error {
		dir := filepath.Join(top, trialTopDir, job)
		if _, err := os.Lstat(dir); err == nil {
			return fmt.Errorf("a Trial system for %s is already there", job)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		for _, sub := range plan.Subvols {
			if err := btrfs("subvolume", "snapshot", filepath.Join(top, sub), filepath.Join(dir, sub)); err != nil {
				return err
			}
		}
		troot := filepath.Join(dir, "@")
		etc := filepath.Join(troot, "etc")
		if err := os.WriteFile(filepath.Join(troot, trialOwnFstab), fstab, 0o644); err != nil {
			return err
		}
		if err := writeSynced(filepath.Join(etc, "fstab"), []byte(plan.Fstab)); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(troot, trialMarker), []byte(job+"\n"), 0o644); err != nil {
			return err
		}
		if plan.ESPTarget != "" {
			if err := copyTree(plan.ESPTarget, filepath.Join(troot, plan.ESPTarget), trialESPCopyMax); err != nil {
				return fmt.Errorf("copying the ESP into the Trial system: %w", err)
			}
		}
		return nil
	})
}

// copyTree copies the regular files and directories under src to dst, at
// most max bytes in all. A FAT ESP has nothing else.
func copyTree(src, dst string, max int64) error {
	var total int64
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if total += fi.Size(); total > max {
			return fmt.Errorf("more than %d bytes", max)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// removeTrials deletes job's Trial system (all: every one lab-agent
// recorded), never the one this boot runs from. It deletes only subvolumes
// under the top level's maclab-trial/<job>/, which lab-agent made.
func (l *Linux) removeTrials(job string, all bool) error {
	var jobs []string
	if all {
		recs, _ := os.ReadDir(filepath.Join(l.WorkDir, trialRecordDir))
		for _, r := range recs {
			jobs = append(jobs, r.Name())
		}
	} else if job != "" {
		if _, err := os.Lstat(l.trialRecord(job)); err == nil {
			jobs = []string{job}
		}
	}
	running := l.RunningTrial()
	jobs = slices.DeleteFunc(jobs, func(j string) bool { return j == running || !reJobName.MatchString(j) })
	if len(jobs) == 0 {
		return nil
	}
	return withTop(func(top string) error {
		var errs []error
		for _, j := range jobs {
			dir := filepath.Join(top, trialTopDir, j)
			subs, _ := os.ReadDir(dir)
			failed := false
			for _, s := range subs {
				p := filepath.Join(dir, s.Name())
				// --recursive: a test may have made subvolumes inside (snapper, a swap subvolume)
				if err := btrfs("subvolume", "delete", "--recursive", p); err != nil {
					if err2 := btrfs("subvolume", "delete", p); err2 != nil {
						errs = append(errs, err)
						failed = true
					}
				}
			}
			if failed {
				continue
			}
			if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
				continue
			}
			os.Remove(l.trialRecord(j))
		}
		os.Remove(filepath.Join(top, trialTopDir)) // only once empty
		return errors.Join(errs...)
	})
}

// RunningTrial is the job whose Trial system this boot runs from, or "".
func (l *Linux) RunningTrial() string {
	root, err := findMount("/")
	if err != nil {
		return ""
	}
	return trialOfSubvol(rootSubvol(root))
}

// ownSystemFiles are the own system's files a Trial run must leave as they were.
var ownSystemFiles = []string{"/etc/pacman.conf", "/etc/fstab", "/etc/mkinitcpio.conf", "/etc/default/limine", "/etc/os-release"}

// OwnSystem fingerprints the Mac's own system. It refuses on a Lab boot:
// only the own system's own boot shows it as it boots.
func (l *Linux) OwnSystem() (api.OwnSystem, error) {
	var o api.OwnSystem
	_, _, cmdline := l.Identity()
	if IsLabBoot(cmdline) {
		return o, errors.New("this is a Lab boot, not the Mac's own system")
	}
	root, err := findMount("/")
	if err != nil {
		return o, err
	}
	o.Root = rootSubvol(root)
	if t := trialOfSubvol(o.Root); t != "" {
		return o, fmt.Errorf("this boot runs from Trial system %s", t)
	}
	o.Digests = map[string]string{}
	pkgs, err := exec.Command("pacman", "-Q").Output()
	if err != nil {
		return o, fmt.Errorf("pacman -Q: %w", err)
	}
	o.Digests["packages"] = sum(pkgs)
	for _, f := range ownSystemFiles {
		if b, err := os.ReadFile(f); err == nil {
			o.Digests[f] = sum(b)
		} else if errors.Is(err, os.ErrNotExist) {
			o.Digests[f] = "absent"
		} else {
			return o, err
		}
	}
	lim, err := detectLimine()
	if err != nil {
		return o, fmt.Errorf("the ESP: %w", err)
	}
	esp, err := espDigest(lim)
	if err != nil {
		return o, err
	}
	o.Digests["esp"] = esp
	// a separate /boot (ext4 with vmlinuz and the initramfs, the ESP at
	// /boot/efi) is not on @, so neither the packages nor the ESP cover it
	if b, err := findMount("/boot"); err == nil && b.Target == "/boot" && b.Target != lim.ESP.Target {
		boot, err := bootDigest(b.Target)
		if err != nil {
			return o, fmt.Errorf("/boot: %w", err)
		}
		o.Digests["boot"] = boot
	}
	return o, nil
}

// espMetadataDirs are what macOS writes on a FAT volume it sees, not the
// Mac's boot setup.
var espMetadataDirs = map[string]bool{".fseventsd": true, ".Spotlight-V100": true, ".Trashes": true}

// espDigest hashes every file on the ESP but the lab's own: its stage
// directory, U-Boot's variable file (the one-shot) and the lab's marked
// block in limine.conf, which come and go with every Lab boot. It skips
// loader/random-seed too, which systemd-boot-random-seed.service rewrites on
// every boot, and macOS's metadata directories.
func espDigest(lim *limineLayout) (string, error) {
	seed := filepath.Join(lim.ESP.Target, "loader", "random-seed")
	return treeDigest(lim.ESP.Target, func(p string, d fs.DirEntry) bool {
		if d.IsDir() {
			return p == lim.Stage || (filepath.Dir(p) == lim.ESP.Target && espMetadataDirs[d.Name()])
		}
		return p == lim.VarFile || p == seed
	}, func(p string, b []byte) []byte {
		if p == lim.Conf {
			return []byte(strings.TrimRight(reMaclabBlock.ReplaceAllString(string(b), "\n"), "\n") + "\n")
		}
		return b
	})
}

// bootDigest hashes every regular file on a separate /boot filesystem. It
// stays on that filesystem: an ESP mounted inside it (/boot/efi) is
// espDigest's. Nothing else is skipped; a Trial run doesn't mount /boot, and
// nothing on it changes across a plain reboot.
func bootDigest(dir string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		return "", err
	}
	dev := st.Dev
	return treeDigest(dir, func(p string, d fs.DirEntry) bool {
		if !d.IsDir() || p == dir {
			return false
		}
		var s unix.Stat_t
		return unix.Lstat(p, &s) == nil && s.Dev != dev
	}, nil)
}

// treeDigest hashes the regular files under root, each with its path relative
// to root, but those skip says to leave out (a directory: all of it).
// transform, if set, may rewrite a file's contents before hashing.
func treeDigest(root string, skip func(p string, d fs.DirEntry) bool, transform func(p string, b []byte) []byte) (string, error) {
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skip(p, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if transform != nil {
			b = transform(p, b)
		}
		rel, _ := filepath.Rel(root, p)
		lines = append(lines, sum(b)+"  "+rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(lines)
	return sum([]byte(strings.Join(lines, "\n"))), nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// checkTrialTest is lab-agent's own guard, whatever labd sends: a Trial
// test runs only on its job's Trial system, never on the Mac's own system,
// and a PR tree comes only with a Trial test.
func checkTrialTest(t api.TestSpec, job, running string) error {
	if t.PR != nil && !t.Trial {
		return fmt.Errorf("test %s: a PR tree is applied only in a Trial system", t.Name)
	}
	if t.Trial && (job == "" || running != job) {
		where := "the Mac's own system"
		if running != "" {
			where = "Trial system " + running
		}
		return fmt.Errorf("test %s runs only in job %s's Trial system, and this boot runs from %s", t.Name, job, where)
	}
	return nil
}

var reCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// prDir is where a Trial run's PR tree is unpacked: inside the Trial system,
// so it goes with it.
func prDir(job string) string { return filepath.Join("/var/tmp/maclab-pr", job) }

// unpackPR fetches a PR tree's archive by sha256 and unpacks it once per
// job, and returns its directory.
func (l *Linux) unpackPR(ctx context.Context, job string, pr api.PRTree, fetch Fetcher) (string, error) {
	if !reCommit.MatchString(pr.Commit) || !reJobName.MatchString(job) {
		return "", fmt.Errorf("bad PR tree %q for job %q", pr.Commit, job)
	}
	dir := prDir(job)
	done := dir + ".done"
	if readTrim(done) == pr.Commit {
		return dir, nil
	}
	os.RemoveAll(dir) // a half unpacked earlier try
	tarball := filepath.Join(l.jobDir(job), "pr-"+pr.Commit[:12]+".tar")
	os.MkdirAll(filepath.Dir(tarball), 0o755)
	if err := fetch(ctx, pr.Archive, tarball); err != nil {
		return "", fmt.Errorf("PR tree %s: %w", pr.Archive, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if out, err := exec.CommandContext(ctx, "tar", "-xf", tarball, "-C", dir, "--no-same-owner").CombinedOutput(); err != nil {
		return "", fmt.Errorf("unpacking the PR tree: %v: %s", err, lastLines(string(out), 5))
	}
	return dir, os.WriteFile(done, []byte(pr.Commit+"\n"), 0o644)
}
