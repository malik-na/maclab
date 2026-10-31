# PR runs and Trial runs: testing any change, and the Mac's way back

Status: implemented and tested against the simulated Mac only. Nothing here
has run on a real Mac yet; see "Not verified" below.

## What it is for

The lab tests kernels by booting them once and returning to the Known-good
kernel. A userspace change (a script that rewrites the package set,
`pacman.conf`, `/etc` or the boot setup) is not undone by a reboot. Testing
one on a lab Mac needs the Mac's own system back exactly as it was
afterwards, whatever the change did and however the test ended.

`lab run <mac> --pr <url|ref>` tests any pull request, branch or commit:

- labd pins it to a commit (`git ls-remote`, as `--kernel <url>` does) and
  fetches only that commit's trees (`--depth 1 --filter=blob:none`).
- A tree with `Kconfig`, `init/main.c` and `arch/arm64/Kconfig` is a kernel:
  the job becomes a source build of that commit and boots it, exactly as
  `--kernel <url>` (no new boot path).
- Anything else is userspace: labd stores `git archive` of the commit as an
  artifact, and the job's `--script` tests run in a Trial system with the
  tree unpacked in `$MACLAB_PR_DIR`. The script decides how to apply and
  test it (`$MACLAB_PR_DIR/install.sh`, `pacman -U`, copying files ...).
- `--pr-kind kernel|userspace` overrides the choice; the job records which
  path it took and why (`result.pr`).

`lab run <mac> --trial --script t.sh` is the same Trial system without a PR.

## The Trial system

Nothing is rolled back. The Mac's own system is never written; the test
runs on a copy of it.

1. Before staging, labd asks lab-agent for a read-only fingerprint of the
   own system (`own_system`): `pacman -Q`, `/etc/pacman.conf`, `/etc/fstab`,
   `/etc/mkinitcpio.conf`, `/etc/default/limine`, `/etc/os-release`,
   every file on the ESP except the lab's own (its stage directory,
   U-Boot's variable file, the lab's marked block in `limine.conf`), and,
   where `/boot` is its own filesystem (ext4 with the kernel and initramfs,
   the ESP at `/boot/efi`), every regular file on it (`boot`). The ESP
   digest also skips `loader/random-seed`, which
   `systemd-boot-random-seed.service` rewrites on every boot, and macOS's
   `.fseventsd`, `.Spotlight-V100` and `.Trashes`. A lab entry left in
   `limine.conf` outside the marked block (an orphan of an earlier Lab boot)
   is part of the fingerprint like any other line.
2. Staging (`stage` with `trial`) mounts the root filesystem's top level
   (`subvolid=5`) privately and snapshots `@`, and each other subvolume
   fstab mounts from it except `/var/log`, to `maclab-trial/<job>/`. It
   rewrites the Trial system's `/etc/fstab`:
   - `/` is `subvol=/maclab-trial/<job>/@`; `/home`, `/var/cache/pacman/pkg`
     and the like are their Trial copies;
   - `/var/log` stays shared, so the Lab boot's journal survives it for
     log collection and lost-boot diagnosis;
   - the ESP is not mounted; the Trial system gets a copy of its files at
     the same path (at most 2 GiB), so a test can rewrite the boot setup
     and the lab can see what it did, without touching the real ESP;
   - swap is off, other disks are left out, pseudo filesystems stay;
   - `@` is mounted at `/run/maclab/own-system` only to bind its
     `/var/lib/maclab` onto `/var/lib/maclab`: lab-agent's state (the Audit
     log, Availability window, Access level, the Lab boot's deadline record)
     stays the Mac's one copy. It takes `/`'s options without
     `x-systemd.growfs`.
   It refuses what it can't place: `/` not on `subvol=@`, a mount of the
   top level, a nested subvolume, a known-good cmdline without exactly one
   `rootflags=...subvol=@` (the boot could then mount `@`), or a bootloader
   other than Limine.
3. The Lab boot is the existing one-shot UKI of the Known-good kernel, with
   `rootflags` pointed at the Trial root, `resume=`/`resume_offset=`
   dropped, and `systemd.gpt_auto=0` added (any `systemd.gpt_auto=` or
   `rd.systemd.gpt_auto=` it had is dropped). Limine sets
   `LoaderDevicePartUUID`, and the Trial fstab has no `/boot` or `/boot/efi`
   entry, so without it `systemd-gpt-auto-generator` could mount the real
   ESP read-write in the Trial system. labd checks the booted cmdline names the Trial root before any
   test runs, and lab-agent refuses a Trial test (`run_test` with `trial`)
   on any boot but its own job's Trial system, whatever labd sends.
4. The tests run. A userspace PR's scripts fetch its tree by sha256.
5. The Lab boot ends the way every Lab boot ends: labd's reboot, the
   no-contact deadline, the hard ceiling, the Availability window, the
   hardware watchdog (a hang), a panic. Every path boots the Mac's default
   entry, which is the untouched own system. If the Trial system can't
   mount its fstab, systemd stops in emergency mode, lab-agent never feeds
   the watchdog, and `watchdog.open_timeout` resets the Mac to its own
   system.
6. Back on the own system, cleanup (labd's restore, and the cleanup every
   job starts with) deletes the job's Trial subvolumes. It deletes only
   under `maclab-trial/<job>/`, only jobs lab-agent recorded in
   `/var/lib/maclab/trials/`, and never the Trial system it runs from.
   Leaving the lab deletes them too.
7. labd reads the fingerprint again and compares. Unchanged: the summary
   says so. Changed: the job fails (`infra_error`) naming what changed, and
   the Mac goes to `new`, out of the pool, until a Maintainer looks.

## What this does and does not protect against

It protects the own system from what a test does through the normal
paths: package managers, hooks, `mkinitcpio`, `limine-update`, editing
`/etc`, a hang or panic mid-change. The tests still run as root. A script
that mounts the disk by hand, writes the ESP's block device, or edits
`/var/lib/maclab` (shared on purpose) can reach the own system; the
fingerprint check catches the paths it covers, not every byte. Lab Macs are
Omacom people's, and a script needs Access level Full on a Volunteer's Mac
as before; isolating a hostile script is not a goal of this change.

`/var/log` and `/var/lib/maclab` are shared, so a test's logs and the lab's
own records are kept. Trial copies of `@home` and the rest are thrown away
with `@`.

## Not verified

None of this has run on a real Mac. Tested only in the simulated lab
(`internal/server/trial_test.go`) and by unit tests of the cmdline and
fstab rewriting (`internal/agent/trial_test.go`). Open on hardware:

- Booting a UKI with `rootflags=subvol=/maclab-trial/<job>/@` on an Omarchy
  Mac: Limine, the initramfs (mkinitcpio's systemd hook) and
  `systemd-remount-fs` honoring it, with the Trial fstab.
- The fstab bind of `/var/lib/maclab` being in place before lab-agent's
  early unit (`After=local-fs.target`) reads the deadline record.
- Omarchy's real fstab and subvolume layout on Apple Silicon matching what
  `planTrial` expects; snapper or `limine-snapper-sync` inside a Trial
  system (the ESP is a plain directory there).
- `btrfs subvolume delete --recursive` on the Macs' btrfs-progs.
- The fingerprint being stable across a plain reboot of the own system
  (nothing else rewriting `limine.conf`, the ESP or a separate `/boot`
  between before and after).
- `systemd.gpt_auto=0` keeping the real ESP unmounted in a Trial boot.
