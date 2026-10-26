# maclab

A test lab for Omarchy Macs. Hand it a kernel, and it boots that kernel exactly
once on a real Apple Silicon Mac. It then runs tests (including Wayland GUI
tests), collects logs, and puts the Mac back on its known-good kernel. When a
kernel crashes or hangs the machine, the lab gets the Mac back itself. When it
can't, it tells a person exactly what to press.

```
lab run m1air --kernel kernel-7.2.0-rc1-lab1.tar.zst --gui-test gui-smoke
```

AI agents get the same operations over MCP (`lab mcp`).

## Pieces

| Binary | Runs on | Job |
|---|---|---|
| `labd` | any Linux box | Controller: devices, leases, job queue, recovery ladder, notifications, artifact and log store |
| `lab-agent` | each test Mac (Omarchy/Asahi, aarch64) | Heartbeat. Stages kernels beside the known-good one, arms the one-shot boot, runs tests, uploads logs |
| `oobd` | the controller host wired to a test Mac's DFU port | Hard reset and serial console. Today: a macOS Mac running `macvdmtool`. Later: a Linux box with a Central Scrutinizer |
| `lab` | your machine | CLI, and `lab mcp` for agents |

## How a job runs

1. **Stage.** The agent downloads the artifact, installs its modules under
   `/usr/lib/modules/<release>` (refusing any release it didn't install
   itself), and writes the boot files for the Mac's bootloader. The
   known-good kernel is never touched.
   - **GRUB:** an initramfs and an entry in `/boot/grub/maclab.cfg`.
   - **Limine** (Omarchy on Apple Silicon: U-Boot, then Limine, then a UKI on
     the ESP): a unified kernel image in `<esp>/maclab/<job>/uki.efi`, listed in
     a marked block at the end of `limine.conf`. Limine reads only FAT, so the
     ESP needs about 120 MB free.
2. **Boot once.** The agent arms a one-shot and reboots. The bootloader
   **clears it before booting the entry**, so any reset afterwards, for any
   reason, lands on the known-good kernel.
   - **GRUB:** `maclab_next=<entry>` in `maclab.env` on the ESP. GRUB can't
     write `grubenv` on btrfs, so a plain `grub-reboot` would never clear;
     that's why the flag lives on the FAT ESP.
   - **Limine:** the Boot Loader Interface variable `LoaderEntryOneShot`, which
     Limine deletes as it reads it. Apple Silicon's U-Boot keeps EFI variables
     in RAM at runtime, so the agent writes U-Boot's store (`VarToFile`) to the
     file U-Boot loads them from (`ubootefi.var` on the ESP).
3. **Watch.** labd waits for a check-in from the new boot. The boot is tagged
   `maclab.job=<id>` on the kernel command line, so labd can tell "booted the
   test kernel" from "fell back". On Macs with an OOB controller, labd also
   reads the serial console and classifies panics, oopses, RCU stalls and
   lockups as they happen.
4. **Test.** `boot-health` always runs. It is followed by any builtin tests
   (`gui-smoke`, `gui-terminal`) or your own scripts. GUI tests run inside the
   GUI user's Hyprland session. Anything written to `$MACLAB_OUT` comes back
   with the job.
5. **Collect.** The journal, kernel log and serial console are saved.
   Warnings and errors that aren't in the Mac's baseline are listed as
   `new_error_lines`.
6. **Restore.** The agent reboots to the known-good kernel, checks that the
   one-shot really cleared, and removes the staged kernel.

A test boot's command line starts from the known-good kernel's and adds the Lab
boot parameters (`panic=10`, `watchdog.open_timeout=N` and the Lab boot marker
`maclab.lab_boot=1`) and `maclab.job=<id>`. It keeps `quiet`, `splash` and `loglevel=`, so the Mac boots
the way its user's boots do; dmesg and the journal record every level anyway. On a Mac
with a serial console, or with `lab run --verbose-boot`, it drops them instead and adds
`loglevel=7` (and the serial console). `lab run --cmdline-base default` starts from the distro's stock
command line (`KERNEL_CMDLINE[default]` in `/etc/default/limine`) instead, to show a
kernel needs no bring-up parameters. `--cmdline-base '<cmdline>'` takes a literal
one. `--cmdline-strip 'asahi.* dcpext_*'` drops matching parameters, and `--cmdline`
appends. The job records the staged and the booted command line. Test boots also
keep `systemd-analyze` time, blame and critical-chain.

Outcomes: `pass`, `tests_failed`, `booted_unhealthy`, `panicked`, `hung`,
`boot_failed`, `lost_contact`, `stage_failed`, `infra_error`, `canceled`.

## Lab boots come back by themselves

Every Lab boot gets the Mac back to its known-good kernel without help, on
every Mac. The one-shot is spent by the time anything below fires, so each
reboot lands on the Mac's own entry.

- `panic=10` reboots a panic, even one before the sysctl applies.
- `watchdog.open_timeout=N` (labd `--watchdog-open-timeout`, default 150s)
  resets an early hang: U-Boot leaves the hardware watchdog running into the
  kernel, `apple_wdt` takes it over, and the kernel stops feeding it unless
  systemd opens it within N seconds (worst case N + 30s). A test kernel must
  be built with `apple_wdt`, or the watchdog resets it about 60s into every
  boot; the job flags one without it.
- Before every Lab boot, lab-agent writes a deadline record to
  `/var/lib/maclab/lab-boot/`: **no contact** (twice the job's boot timeout,
  unless lab-agent reaches labd first) and a **hard ceiling** (the job's end).
  `maclab-lab-boot-deadlines.service`, an early unit with no network
  dependency, reboots a Lab boot when either passes, and on a Volunteer's Mac
  as soon as its Availability window closes. labd waits for that
  before its recovery ladder, so a Lab-owned Mac's OOB hard resets are a later rung.
- After any recovery, lab-agent uploads the lost boot's journal (by boot ID)
  and any pstore/ramoops records, as `prevboot/` in the job.
- A boot that came back only through its deadline fails as `lost_contact`:
  the kernel or Release broke networking.

[docs/lab-boot-hardware-check.md](docs/lab-boot-hardware-check.md) is the
check that the early unit and the watchdog work on a real Mac.

## When things go wrong

labd notices a problem from any of these signals:

- The heartbeat stops.
- The boot ID doesn't change after a reboot.
- The Mac comes back on the known-good kernel.
- A panic appears on serial, and the Mac doesn't reboot within 60s.
- The serial console goes silent mid-boot for 2 minutes.

It then climbs a ladder:

1. Soft reboot through the agent, if the agent still answers.
2. Hard reset through the OOB controller, up to twice.
3. **Ask a human.** The Mac goes to `needs_hands`, and one urgent
   notification goes out (ntfy and/or a command such as `notify-send`):

   > **m1air needs a power cycle**
   > 1. Find m1air (MacBook Air M1, left desk). Open the lid if it's closed.
   > 2. Hold the power button (the Touch ID key, top right of the keyboard) for 10 seconds, until the screen stays black.
   > 3. Wait 5 seconds, then press it once to turn it back on.
   > 4. That's it. The lab notices when it checks in again and picks up from there.

On a Volunteer's Mac the ladder differs: nothing reaches the Volunteer until
the Lab boot's Hard ceiling plus one boot timeout have passed with no check-in
(by then its own deadlines have had every chance to bring it back), and the
notice goes only to the Mac's own ntfy topic, never the lab-wide topic or
`--notify-cmd`, which are the Maintainers'. `sudo lab-agent notify --topic
https://ntfy.sh/my-topic` switches the Mac to the Volunteer's own topic; it
reaches labd at the next check-in, and `status` shows the `subscribe to:` URL.

When the Mac checks in again, the alert clears, leftover staged kernels are
removed, and queued jobs continue. `lab status` shows the same instruction
while it's outstanding.

Each Mac gets a recovery tier: 0 = panic reboot + one-shot fallback and the
Lab boot deadlines, 1 = + hardware watchdog (`apple_wdt`, armed through systemd),
2 = + out-of-band reset and serial.

## Setup

```sh
make                                 # binaries in bin/
labd --maintainer <you>              # first start: you become the first Maintainer (default $USER)
lab login http://<labd-host>:7770 "$(cat ~/.local/share/maclab/admin.token)"
lab enroll m1air --label "MacBook Air M1, left desk"
```

Each Maintainer has their own labd token. `lab maintainer add <name>` prints
a new one once (labd keeps only its hash); `lab maintainer list` and
`lab maintainer remove <name>` do the rest, and removing someone revokes their
token at once. Jobs and Promotions record the Maintainer whose token asked.
Upgrading from a labd with a single admin token keeps that token, in the same
`admin.token` file, as the first Maintainer's (`--maintainer`, default `$USER`).

labd listens on `127.0.0.1:7770` unless told otherwise. For Macs over Tailscale,
start it with `--listen <tailnet-ip>:7770` (or `MACLAB_LISTEN`) so it is not
reachable off the tailnet. Remote links are slow, so the timeouts that assume a
LAN are flags: `--boot-timeout`, `--offline-after`, `--alive-window`,
`--quiet-grace` and `--stage-timeout` (defaults are sized for remote Macs).
Artifact downloads resume with HTTP range requests after a dropped transfer. An
enrollment token is spent by the Mac's first check-in, so a failed `setup` can
be run again with the same token until it expires. lab-agent's own request
timeouts are `http_timeout_sec` and `transfer_timeout_sec` in its config file.

On the Mac:

```sh
sudo ./lab-agent setup --server http://<labd-host>:7770 --token enr_... --gui-user <you> --autologin
```

`setup` makes these changes. Each one is idempotent and backed up:

- GRUB: appends the one-shot hook to `/boot/grub/custom.cfg` (backup: `custom.cfg.pre-maclab`) and creates `maclab.env` on the ESP.
- Limine: creates `<esp>/maclab/`, adds the (empty) maclab block to `limine.conf`, and keeps a copy of U-Boot's `ubootefi.var` as `ubootefi.var.pre-maclab`.
- Records the running kernel as known-good.
- Sets `kernel.panic = 10`.
- Arms the hardware watchdog (`RuntimeWatchdogSec=30s`).
- Installs and enables `maclab-lab-boot-deadlines.service`, which reboots a Lab boot that misses its deadline.
- With `--autologin`, logs the GUI user into the desktop on boot; with `--autologin` or `--stay-awake`, turns their idle lock and screensaver off (Lab-owned Macs only).
- Installs and starts `lab-agent.service`.

Then run the baseline:

```sh
lab baseline m1air    # two reboots: proves the one-shot selects AND clears
lab crashtest m1air   # optional: deliberate panic, checks it recovers by itself
```

No other job runs on a Mac until its baseline passes.

### A Volunteer's Mac

A Mac is Lab-owned or a Volunteer's for as long as it stays enrolled; the
enrollment token decides which, and changing it means enrolling again.

```sh
lab enroll ada-mbp --volunteer "Ada Lovelace" --contact ada@example.org
```

Either flag without the other is refused. labd gives the Mac its own
unguessable ntfy topic when it enrolls (on the server `--ntfy` posts to), and
`setup` prints it with a QR code to subscribe from a phone. On a Volunteer's
Mac `setup` refuses `--autologin` and `--stay-awake`. Setup and every
check-in report whether `@` can be snapshotted (`/` is the btrfs subvolume
`@` and snapper has a root config for it); a Mac whose `@` can't is shown as
not eligible for Installed runs. On the Mac, `sudo lab-agent status` shows
what it is enrolled as, its topic, its Availability window and its
eligibility.

Nothing from the lab starts on a Volunteer's Mac outside its Availability
window, the baseline included, and a Mac with no window is closed. Only the
Volunteer sets it, on the Mac, in the Mac's local time:

```sh
sudo lab-agent window add mon-fri 19:00-07:00   # a window crossing midnight belongs to the day it starts
sudo lab-agent window remove mon-fri 19:00-07:00
sudo lab-agent window show
sudo lab-agent pause                     # end the current window now
sudo lab-agent pause --until 2026-11-03  # closed until that date begins
sudo lab-agent open --until 22:00        # open now until 22:00, whatever the schedule says
```

These take effect at once and work with labd or the tailnet down; check-ins
carry them to labd, which has no API to change them. lab-agent refuses any lab
command outside the window, and the job records the refusal. labd starts a job
on a Volunteer's Mac only inside its window, on AC power, and when the job's
expected length fits in what is left of the window; until then the job stays
`queued`. `--omt-every` refreshes wait for a window the same way. A Lab boot's
hard ceiling is the window's end if that comes before the job's end.

At the window's end, or on `pause`, lab-agent stops the job itself, on its own
clock and with labd or the tailnet down too: staging stops where it is, and a
Lab boot (one that hasn't reached labd yet included) reboots to the Known-good
kernel within a second or two. The job ends `incomplete`, not failed, and labd
queues it again as a new job for a later window. A lease on a Volunteer's Mac
ends with the window at the latest, and `pause` ends it. While a job holds the
Mac, lab-agent blocks suspend (`systemd-inhibit --what=sleep --mode=block`)
and releases it after; the idle lock and screensaver are never touched.
Nothing wakes a sleeping Mac: a window it sleeps through is missed.

### Out-of-band control from a macOS host

On the controller Mac, build `macvdmtool` (Xcode command line tools).
Connect its DFU port to the test Mac's DFU port with a USB-C cable that isn't
USB 2.0-only.

**Step zero:** check that `sudo macvdmtool nop` reports a connection and that
`sudo macvdmtool reboot serial` resets the target. Only then install `oobd` using
`deploy/com.maclab.oobd.plist`, and point labd at it:

```sh
lab device m1air --oob-url http://<controller-host>:7780 --oob-token <token>
lab oob m1air
```

`macvdmtool` only drives one port, so it's one controller Mac per test Mac.

## Live data: stream, screen, logs, shell

Everything a UI needs is on the API. The web UI uses it now, and a TUI can use it later.

- `GET /api/stream`: server-sent events for `device`, `job`, `serial`, `kevent` and `screen` (`?device=` limits it to one Mac).
- `POST /api/devices/{n}/screenshot`, `GET .../screen`, `.../screens`: captures come from the display
  controller via `ffmpeg kmsgrab`. That works from the moment the display driver loads (boot splash,
  text console, emergency shell, login screen, desktop), with `grim` as a fallback. Jobs capture
  `screen/after-boot.jpg` and `screen/after-tests.jpg`.
- `GET /api/devices/{n}/logs?source=journal|kernel&lines=N`: live from the Mac.
- `lab shell <device>`: an interactive shell over the out-of-band USB-C serial link (`GET/POST
  /api/devices/{n}/console`). It works with the Mac's network down. `lab-agent setup` enables an
  115200 getty on `ttySAC0`.

Before the display driver loads, the screen can't be captured, but serial carries the text of every
stage: m1n1, U-Boot, the kernel from `[0.000000]` (lab boots add `console=ttySAC0,115200`), and
login. A Mac leaves serial mode on every reboot, so labd asks oobd to re-arm around each one.

## Build from a GitHub URL

```sh
lab run m1air --kernel https://github.com/iconidentify/aurora-linux/tree/custom/sep --gui-test gui-smoke
lab build https://github.com/AsahiLinux/linux/pull/123 --device m1air
lab build https://github.com/AsahiLinux/linux/pull/123 --config ./my.config   # one build for every Mac
```

Agents do the same with `lab_run(source=...)`, `lab_build` and `lab_build_status`, and use `lab_exec`
instead of ssh. labd resolves repo, branch (`/tree/...`), commit and PR URLs (or `git-url#ref`) to
a commit with `git ls-remote`. It keys the build on that commit, the target Mac's running
`/proc/config.gz` and the kbuild recipe, and reuses an identical earlier build.

`lab-builder` runs on the controller Mac as a root LaunchDaemon. Root is needed because macOS Local
Network privacy blocks user processes. It runs `builder/kbuild` in an Apple `container` micro-VM
(`builder/Containerfile`: Debian, gcc, Rust 1.93.1, bindgen), started in the user's session. All
state is one size-capped volume (`container volume create -s 40G maclab-cache`): one shared
depth-1 git store, one reused worktree, at most two build dirs, and an 8 GB ccache. The host scratch
dir is emptied after every build. labd's artifact store is garbage-collected to a budget
(`--artifact-budget-gb`, default 20).

Measured on a base M5 (10 cores, VM with 16 GB) with aurora's config: first build 9 minutes;
rebuild of a warm tree about 40 seconds. Progress (stage, object count, ETA) and the log stream
live on `/api/stream` (`build`, `buildlog`) and `/api/builds/{id}/log?follow=1`.

## omarchy-m-test: hardware checks in every job

On a Mac with a desktop user, every job also runs
[omarchy-m-test](https://github.com/maralcbr/omarchy-m-testing) in the desktop
session: about 70 checks of the boot chain, drivers, GPU, video decode, display,
audio, Wi-Fi, power and CPU, and short benchmarks. The agent installs or upgrades
the tool with its own installer, which checks the release signature. It answers
the disclaimer and then gives no more input, so every question meant for a person
(look at the screen, close the lid, reload Wi-Fi, upload) is skipped, never
answered yes. The run takes 1 to 3 minutes.

- **Known-good runs** (`lab baseline`, `lab run <mac>` with no kernel) become the
  Mac's reference report.
- **Lab kernels** are compared with that reference. A check that passes on the
  known-good kernel and fails on the test kernel is a regression and fails the job.
  `boot.files` and `boot.kernel-package` are listed apart: they fail for any
  kernel that isn't an installed package. A kernel that lacks a feature on
  purpose names what it's expected to fail: `--omt-allow system.failed-units`,
  or `--omt-allow hardware.drivers:<compatible>` for one device-tree node, which
  still fails the job if any other node stops binding.
- **Publishing:** `lab run <mac> --publish`, `lab publish <job>`, or the job page's
  Publish button uploads the signed report to omarchy-m-testing.org. Only
  known-good runs qualify, because the site names a report's build from the
  installed packages, and those don't describe a lab kernel. The site's deletion
  link stays in the job's `omt-published.json`.

```sh
labd --omt-publish --omt-every 24h   # refresh each idle Mac's known-good report daily, and publish it
labd --omt=false                     # don't add it to jobs
```

## Release packages from a PKGBUILD

```sh
lab package ~/recipes/linux-aurora                        # the recipe exactly as written
lab package ~/recipes/linux-aurora --pkgrel 11.14 \
    --commit https://github.com/iconidentify/aurora-linux/commit/<sha> -o ./rel
lab run m1air --kernel <build-id>                         # boot the kernel package once
lab build-get <build-id> -o ./rel                         # packages + the PKGBUILD as built, sha256-checked,
                                                          # and the uploaded recipe (patches, data files) in ./rel/recipe/
```

A kernel build (above) makes a lab test artifact. A package build makes the release: it runs a
PKGBUILD recipe with `makepkg`, the way the release machine (m1) does, and returns every package it
makes (`linux-aurora`, `linux-aurora-headers`, ...) plus the PKGBUILD as built. The CLI uploads only
the recipe's own files: the PKGBUILD, its local sources, and install/changelog files. makepkg
downloads the rest. `--commit` repoints `_commit` (the PKGBUILD must download from that repository),
`--pkgrel` sets pkgrel, and either one refreshes the checksums with `updpkgsums`. Agents use
`lab_package`, then `lab_build_status`, then `lab_run(build_id=...)`.

The builder is `builder/pkg`. It is Arch Linux ARM, the release machine's `/etc/makepkg.conf`
verbatim, and `maclab.conf` with the one difference: packages are `.pkg.tar.zst`. ccache stays
off, as on the release machine: its base-dir path rewriting changes `__FILE__` strings inside
modules. When the image is built, gcc, glibc, binutils, rustup, bindgen and pahole come out at the
same versions as on an up-to-date Arch Linux ARM machine. Rebuild the image to follow the distribution:

```sh
# on the controller Mac, in a directory holding builder/pkg/* and the rootfs tarball
curl -LO http://os.archlinuxarm.org/os/ArchLinuxARM-aarch64-latest.tar.gz
container build -t maclab-pkgbuild:1 .
container volume create -s 120G maclab-pkgcache   # once; a kernel tree needs ~35 GB while building
```

Checked against a linux-aurora build of the same recipe on an Arch Linux ARM machine: the file
lists and package metadata are identical. Contents are byte-identical except where the build
directory's path leaks in: ELF build-ids, the source paths Rust drivers embed for panics
(`/cache/work/b/...` here), and the build timestamp in `vmlinuz`. A cold build takes 8 to 12
minutes on a base M5.

The 30+ GB work tree is deleted after every build. The volume keeps downloaded sources (the newest
six) and the Rust toolchains. `lab-builder` takes package builds only when it has
`--pkg-image` (the default, `maclab-pkgbuild:1`). An older builder only ever gets kernel builds.

## Kernel artifacts

A tarball in the Arch package layout: exactly one
`usr/lib/modules/<release>/` holding the modules and `vmlinuz` (or `Image`).
A `linux-*.pkg.tar.zst` works as-is. From an `O=` build tree:

```sh
lab pack --build ~/linux/build    # the O= dir -> kernel-<release>.tar.zst
```

The release string must be unique (set `LOCALVERSION`). The agent refuses to
install over a kernel it didn't install, including the running one.

The device tree comes from m1n1 on the ESP, not from the test kernel, and that includes a
release package booted with `lab run`. A kernel that needs new DT bindings needs a matching
m1n1 stage 2. A lab boot doesn't exercise device-tree changes.

## For AI agents

```sh
claude mcp add maclab -- lab mcp
```

Tools: `lab_devices`, `lab_device`, `lab_run`, `lab_wait`, `lab_job`, `lab_jobs`,
`lab_log`, `lab_screenshot`, `lab_baseline`, `lab_crashtest`, `lab_lease`,
`lab_release`, `lab_reset`, `lab_serial`, `lab_exec`, `lab_cancel`, `lab_build`,
`lab_build_status`, `lab_package` and `lab_publish`. A device in `needs_hands` is waiting for a
person. Tell the user; don't retry.

A lab is worth describing to its agents once: which Macs you have, which one has
serial, what they must not touch. Write that in a short Markdown file and hand it
to them with a prompt.

## Not yet verified on hardware

- `macvdmtool` driving the target from an M3 host. (It is verified from an **M5 Pro** host, J704AP, on macOS 26.5.)
- Whether `apple_wdt` can be armed before the kernel starts (m1n1/U-Boot).
  If it can, tier 1 would also cover early-boot hangs.

## Development

`make test` runs the unit tests and eight end-to-end scenarios against a
simulated Mac (`internal/agent/sim.go`):

- A good kernel
- Panic at boot
- Hang, recovered by hard reset
- Silent hang, caught from serial
- No OOB, human power cycle, then queued jobs resume
- Crash during a test
- Deliberate crash test
- A failing test

`lab-agent sim --server .. --token ..` enrolls a simulated Mac against a real labd.

## Phones

Open `http://<labd-host>:7770` and enter your Maintainer token; the UI keeps
it. Every client needs a token, whatever network it's on. On a phone the UI
switches to a bottom tab bar and can be added to the home screen.

## License

MIT; see [LICENSE](LICENSE). The web UI bundles third-party files under their own
licenses: xterm.js (MIT), subsets of JetBrains Mono Nerd Font and IBM Plex Sans
(SIL OFL 1.1), and the Omarchy logo, icon and font (MIT). See
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). maclab isn't an official Omarchy
or Asahi Linux project, and it isn't affiliated with Apple.
