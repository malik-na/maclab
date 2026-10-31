package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iconidentify/maclab/internal/api"
)

func TestTrialCmdlineBootsTheTrialSystem(t *testing.T) {
	got, err := trialCmdline("root=UUID=abc rw rootflags=subvol=@,compress=zstd:3 quiet resume=UUID=abc resume_offset=533760 maclab.job=j1", "j1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "rootflags=subvol=/maclab-trial/j1/@,compress=zstd:3") {
		t.Errorf("rootflags not pointed at the Trial system: %s", got)
	}
	if strings.Contains(got, "resume") {
		t.Errorf("a Trial system's boot must never resume from the own system's swap: %s", got)
	}
	if !strings.Contains(got, "root=UUID=abc rw") || !strings.Contains(got, "maclab.job=j1") {
		t.Errorf("the rest of the cmdline changed: %s", got)
	}
	if !strings.HasSuffix(got, " systemd.gpt_auto=0") || strings.Count(got, "gpt_auto") != 1 {
		t.Errorf("a Trial system's boot must not let systemd-gpt-auto-generator mount the real ESP: %s", got)
	}
	// a gpt_auto the known-good cmdline already has can't turn it back on
	got, err = trialCmdline("root=UUID=abc rootflags=subvol=@ systemd.gpt_auto=1 rd.systemd.gpt_auto=1", "j1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, "gpt_auto") != 1 || !strings.HasSuffix(got, " systemd.gpt_auto=0") {
		t.Errorf("gpt_auto not forced off: %s", got)
	}
	if trialOfSubvol("/maclab-trial/j1/@") != "j1" || trialOfSubvol("/@") != "" || trialOfSubvol("/maclab-trial/j1/@home") != "" {
		t.Error("trialOfSubvol")
	}
}

func TestTrialCmdlineRefusesWhatCouldMountTheOwnSystem(t *testing.T) {
	for _, c := range []struct{ cmdline, job string }{
		{"root=UUID=abc rw", "j1"},                                    // the default subvolume: could be @
		{"root=UUID=abc rootflags=compress=zstd", "j1"},               // no subvol
		{"root=UUID=abc rootflags=subvol=@home", "j1"},                // not @
		{"root=UUID=abc rootflags=subvolid=256", "j1"},                // by id
		{"root=UUID=abc rootflags=subvol=@ rootflags=subvol=@", "j1"}, // which one wins?
		{"root=UUID=abc rootflags=subvol=@", "../j1"},                 // a path, not a job
		{"root=UUID=abc rootflags=subvol=@", ""},                      // no job
	} {
		if got, err := trialCmdline(c.cmdline, c.job); err == nil {
			t.Errorf("%q (job %q) was accepted as %q", c.cmdline, c.job, got)
		}
	}
}

// omarchyFstab is an Omarchy-shaped fstab, with a swapfile, a second disk and a tmpfs.
const omarchyFstab = `# /dev/nvme0n1p6
UUID=abc / btrfs rw,relatime,compress=zstd:3,subvol=/@ 0 0
UUID=abc /home btrfs rw,relatime,compress=zstd:3,subvol=/@home 0 0
UUID=abc /var/log btrfs rw,relatime,compress=zstd:3,subvol=/@log 0 0
UUID=abc /var/cache/pacman/pkg btrfs rw,relatime,compress=zstd:3,subvol=/@pkg 0 0
UUID=ESP1 /boot vfat rw,relatime,fmask=0077,dmask=0077 0 2
/swap/swapfile none swap defaults 0 0
UUID=other /mnt/data ext4 defaults 0 2
tmpfs /tmp tmpfs defaults,noatime,mode=1777 0 0
`

func TestPlanTrialMountsNothingOfTheOwnSystemButLabState(t *testing.T) {
	p, err := planTrial(omarchyFstab, "/boot", "j1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"@", "@home", "@pkg"}; !slices.Equal(p.Subvols, want) {
		t.Errorf("snapshots %v, want %v", p.Subvols, want)
	}
	var mounted []string
	for _, line := range strings.Split(p.Fstab, "\n") {
		e, ok := parseFstabLine(line)
		if !ok {
			continue
		}
		mounted = append(mounted, e.target)
		sub := optSubvol(e.opts)
		switch {
		case e.target == "/" && sub != "/maclab-trial/j1/@":
			t.Errorf("/ is %s, not the Trial system", sub)
		case e.target == "/home" && sub != "/maclab-trial/j1/@home":
			t.Errorf("/home is %s, not its Trial copy", sub)
		case e.spec == "UUID=abc" && !strings.HasPrefix(sub, "/maclab-trial/j1/") && e.target != "/var/log" && e.target != trialOwnMount:
			t.Errorf("%s mounts the own system's %s read-write", e.target, sub)
		case e.target == trialOwnMount && sub != "/@":
			t.Errorf("the own-system mount is %s", sub)
		case e.target == trialStateDir && (e.spec != trialOwnMount+trialStateDir || !strings.Contains(e.opts, "bind")):
			t.Errorf("lab-agent's state is not the own system's: %s", line)
		}
	}
	for _, off := range []string{"/boot", "none", "/mnt/data"} {
		if slices.Contains(mounted, off) {
			t.Errorf("%s is still mounted in the Trial system:\n%s", off, p.Fstab)
		}
	}
	for _, on := range []string{"/", "/home", "/var/log", "/var/cache/pacman/pkg", "/tmp", trialOwnMount, trialStateDir} {
		if !slices.Contains(mounted, on) {
			t.Errorf("%s is not mounted in the Trial system:\n%s", on, p.Fstab)
		}
	}
	if !strings.Contains(p.Fstab, "# maclab Trial system: the ESP is not mounted") {
		t.Errorf("the ESP line is not commented out with a reason:\n%s", p.Fstab)
	}
}

func TestPlanTrialOwnSystemMountDoesNotGrowfs(t *testing.T) {
	fstab := "UUID=abc / btrfs rw,x-systemd.growfs,compress=zstd:3,subvol=/@ 0 0\n"
	p, err := planTrial(fstab, "/boot", "j1")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(p.Fstab, "\n") {
		if e, ok := parseFstabLine(line); ok && e.target == trialOwnMount {
			if strings.Contains(e.opts, "growfs") || e.opts != "rw,compress=zstd:3,subvol=/@" {
				t.Errorf("the own-system mount's options are %q", e.opts)
			}
			return
		}
	}
	t.Errorf("no own-system mount:\n%s", p.Fstab)
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestESPDigestIgnoresWhatEveryBootRewrites(t *testing.T) {
	esp := t.TempDir()
	lim := &limineLayout{ESP: mount{Target: esp}, Conf: filepath.Join(esp, "limine.conf"),
		Stage: filepath.Join(esp, "maclab"), VarFile: filepath.Join(esp, "ubootefi.var")}
	writeFile(t, filepath.Join(esp, "EFI/BOOT/BOOTAA64.EFI"), "limine")
	writeFile(t, lim.Conf, "timeout: 3\n")
	writeFile(t, filepath.Join(esp, "loader/random-seed"), "seed 1")
	digest := func() string {
		t.Helper()
		d, err := espDigest(lim)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	before := digest()
	writeFile(t, filepath.Join(esp, "loader/random-seed"), "seed 2")
	writeFile(t, lim.VarFile, "one-shot")
	writeFile(t, filepath.Join(lim.Stage, "j1.efi"), "uki")
	writeFile(t, filepath.Join(esp, ".fseventsd/0001"), "x")
	writeFile(t, filepath.Join(esp, ".Spotlight-V100/store"), "x")
	writeFile(t, filepath.Join(esp, ".Trashes/501/x"), "x")
	if after := digest(); after != before {
		t.Error("the ESP digest changed with only random-seed, the lab's own files and macOS metadata")
	}
	writeFile(t, filepath.Join(esp, "EFI/BOOT/BOOTAA64.EFI"), "limine 2")
	if digest() == before {
		t.Error("the ESP digest missed a changed boot loader")
	}
	writeFile(t, filepath.Join(esp, "EFI/BOOT/BOOTAA64.EFI"), "limine")
	writeFile(t, filepath.Join(esp, "sub/loader/random-seed"), "x")
	if digest() == before {
		t.Error("the ESP digest skipped a random-seed outside loader/")
	}
}

func TestBootDigestCoversASeparateBoot(t *testing.T) {
	boot := t.TempDir()
	writeFile(t, filepath.Join(boot, "vmlinuz-linux-asahi"), "kernel")
	writeFile(t, filepath.Join(boot, "initramfs-linux-asahi.img"), "initramfs")
	before, err := bootDigest(boot)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := bootDigest(boot); again != before {
		t.Error("the /boot digest is not stable")
	}
	writeFile(t, filepath.Join(boot, "initramfs-linux-asahi.img"), "initramfs 2")
	if after, _ := bootDigest(boot); after == before {
		t.Error("the /boot digest missed a rebuilt initramfs")
	}
	writeFile(t, filepath.Join(boot, "initramfs-linux-asahi.img"), "initramfs")
	writeFile(t, filepath.Join(boot, "vmlinuz-linux-test"), "kernel")
	if after, _ := bootDigest(boot); after == before {
		t.Error("the /boot digest missed a new kernel")
	}
}

func TestPlanTrialRefusesWhatItCantPlace(t *testing.T) {
	for name, fstab := range map[string]string{
		"no /":          "UUID=abc /home btrfs subvol=/@home 0 0\n",
		"/ is not @":    "UUID=abc / btrfs subvol=/@root 0 0\n",
		"the top level": "UUID=abc / btrfs subvol=/@ 0 0\nUUID=abc /mnt/top btrfs subvolid=5 0 0\n",
		"nested":        "UUID=abc / btrfs subvol=/@ 0 0\nUUID=abc /srv btrfs subvol=/@/srv 0 0\n",
		"@ twice":       "UUID=abc / btrfs subvol=/@ 0 0\nUUID=abc /again btrfs subvol=@ 0 0\n",
		"/ twice":       "UUID=abc / btrfs subvol=/@ 0 0\nUUID=abc / btrfs subvol=/@ 0 0\n",
	} {
		if p, err := planTrial(fstab, "/boot", "j1"); err == nil {
			t.Errorf("%s: planned\n%s", name, p.Fstab)
		}
	}
	if _, err := planTrial(omarchyFstab, "/boot", "j/1"); err == nil {
		t.Error("a job id with a slash was accepted")
	}
}

func TestTrialTestRunsOnlyInItsTrialSystem(t *testing.T) {
	trial := api.TestSpec{Name: "t", Script: "s", Trial: true}
	pr := api.TestSpec{Name: "t", Script: "s", Trial: true, PR: &api.PRTree{Commit: strings.Repeat("a", 40)}}
	for _, c := range []struct {
		t            api.TestSpec
		job, running string
		ok           bool
	}{
		{trial, "j1", "j1", true},
		{pr, "j1", "j1", true},
		{trial, "j1", "", false},   // the own system
		{trial, "j1", "j2", false}, // another job's
		{trial, "", "", false},
		{api.TestSpec{Name: "t", Script: "s", PR: pr.PR}, "j1", "j1", false}, // a PR tree outside a Trial test
		{api.TestSpec{Name: "t", Script: "s"}, "j1", "", true},               // not a Trial test
	} {
		if err := checkTrialTest(c.t, c.job, c.running); (err == nil) != c.ok {
			t.Errorf("trial=%v pr=%v job %q on %q: %v", c.t.Trial, c.t.PR != nil, c.job, c.running, err)
		}
	}
}

func TestOwnSystemDiffNamesWhatChanged(t *testing.T) {
	a := api.OwnSystem{Root: "/@", Digests: map[string]string{"packages": "1", "esp": "2"}}
	if d := a.Diff(a); d != nil {
		t.Errorf("a fingerprint differs from itself: %v", d)
	}
	b := api.OwnSystem{Root: "/@", Digests: map[string]string{"packages": "9", "esp": "2", "/etc/pacman.conf": "3"}}
	if d := a.Diff(b); !slices.Equal(d, []string{"/etc/pacman.conf", "packages"}) {
		t.Errorf("diff %v", d)
	}
	if d := a.Diff(api.OwnSystem{Root: "/maclab-trial/j1/@", Digests: a.Digests}); len(d) != 1 || !strings.HasPrefix(d[0], "root") {
		t.Errorf("a different root: %v", d)
	}
}
