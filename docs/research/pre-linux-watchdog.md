# Can a Lab boot recover by itself from a hang before Linux starts?

Researched 2026-10-29 after josip-omarchy (M3 Pro 14", t6030) went dark on its
first Lab boot (`j1028-221602-0bef`) and m3pro (M3 Pro 16", t6030) hung at m1n1's
"Vectoring to next stage..." (`j1004-102226-f4c0`). Follows on from
[apple-watchdog-before-kernel.md](apple-watchdog-before-kernel.md) (#13), which covers
U-Boot handing the watchdog to Linux.

**Short answer: no, not with the stock Asahi boot chain, and no setting changes that.**
Before Linux, the hardware watchdog is off from m1n1 stage 1's init until U-Boot
starts it, and m1n1 has no option to leave it on. Once U-Boot starts it, U-Boot feeds it
every time U-Boot or Limine waits for something: a key, a menu, or the U-Boot prompt.
So a hang in either place lasts until someone power cycles the Mac. Closing the first gap
needs a patched m1n1 **stage 1**, which only a person can install, in 1TR, once per Mac.
Closing the second needs patched U-Boot or Limine builds. Even then, m3pro's one
recorded pre-Linux hang came back after an OOB hard reset (a full SoC reset, as far as we
can tell). So a watchdog reset might only have turned that hang into a reboot loop.

Section 4 has the proposal. In short: confirm on m3pro that the watchdog works there at
all, clear out the waits maclab can cause itself, and gate Volunteer Lab boots on the same
boot chain having passed on a Lab-owned Mac of the same chip. The real fix needs upstream
m1n1 and Limine changes, so those are listed as long-term work.

Sources pinned to:
m1n1 [`c4c2fed8`](https://github.com/AsahiLinux/m1n1/tree/c4c2fed8f894da5f41e59ae0d313621fa58804a1)
(main, 2026-10-28; the watchdog, chainload and payload code is the same as tag
`v1.6.1` = `06a4601a`, apart from the licence header);
Asahi U-Boot [`b33034a5`](https://github.com/AsahiLinux/u-boot/tree/b33034a515a068381c4aa28831fb709e91300cd1)
(branch `asahi`; m3pro reports "BIOS 2026.07");
Limine [`89a9512c`](https://github.com/limine-bootloader/limine/tree/89a9512c623a4779cc73e3869383876d54264b28)
(v12.9.6+, 2026-10-28; **Omarchy's installed Limine version was not checked**);
Linux [`c339f401`](https://github.com/torvalds/linux/tree/c339f4010dea8b0d754f84ace70cd1b089878ef8);
AsahiLinux/docs [`5acb80f6`](https://github.com/AsahiLinux/docs/tree/5acb80f669ba424b57207a1016b7a64a2ada783c);
asahi-installer [`e93743d5`](https://github.com/AsahiLinux/asahi-installer/tree/e93743d52178b3e26e428fa05174ef549abf30cb).

"Verified" below means read in the pinned source or seen in a labd log. "Inferred" means
reasoned from those, not seen on hardware.

## The pre-Linux chain and who holds the watchdog

| Window | Watchdog | Hang here resets by itself? |
| --- | --- | --- |
| iBoot → m1n1 stage 1 up to its `wdt_disable()` | iBoot's, ~120 s | Yes (Asahi docs; not seen on hardware) |
| A: stage 1 `wdt_disable()` → chainload → **stage 2** m1n1 → U-Boot's early init | **off** | **No.** j1004 hung here |
| B: U-Boot `initr_watchdog()` → U-Boot / Limine **busy** (a loop that doesn't call `schedule()`), Limine "System halted" | U-Boot's, 60 s, not fed | Yes, within 60 s (inferred) |
| C: U-Boot / Limine **waiting** (U-Boot prompt, Limine menu with no timeout, Limine "Press a key") | U-Boot's, **fed while it waits** | **No** |
| ExitBootServices → `apple_wdt` probe | U-Boot's, last fed at exit | Yes, ≤ 60 s ([#13](apple-watchdog-before-kernel.md)) |

The hardware limit applies to any fix: the counter is 32-bit at 24 MHz, so the longest
timeout is 2³² / 24 MHz ≈ **178 s**
([docs wdt.md:9-13](https://github.com/AsahiLinux/docs/blob/5acb80f669ba424b57207a1016b7a64a2ada783c/docs/hw/soc/wdt.md?plain=1#L9-L13)).

## 1. m1n1: where it disables the watchdog, and whether anything can stop it

### Verified (source)

- `m1n1_main()` calls `wdt_disable()` unconditionally after MMU/AIC/SMP init, before
  `pmgr_init()` and long before `run_actions()` picks the payload
  ([main.c:168](https://github.com/AsahiLinux/m1n1/blob/c4c2fed8f894da5f41e59ae0d313621fa58804a1/src/main.c#L168)).
  It sits outside every `#ifdef`, so `BRINGUP` builds run it too. It then prints
  "Vectoring to next stage..." and jumps
  ([main.c:211-213](https://github.com/AsahiLinux/m1n1/blob/c4c2fed8f894da5f41e59ae0d313621fa58804a1/src/main.c#L211-L213)).
  Stage 1 and stage 2 are the same program, so **both** disable it.
- `wdt_disable()` writes 0 to the primary control register (`+0x1c`), and on
  `wdt-version` 2 or 3 also writes 0 to the secondary watchdog at ADT `reg[2]`
  ([wdt.c:14-49](https://github.com/AsahiLinux/m1n1/blob/c4c2fed8f894da5f41e59ae0d313621fa58804a1/src/wdt.c#L14-L49)).
  The m3pro log shows both: `Primary WDT register @ 0x3642b0000 / Primary WDT disabled /
  Secondary WDT register @ 0x3642b8008 / Secondary WDT disabled`.
- The only other watchdog code, `wdt_reboot()`, arms it with a ~44 ms alarm to reboot at
  once ([wdt.c:51-59](https://github.com/AsahiLinux/m1n1/blob/c4c2fed8f894da5f41e59ae0d313621fa58804a1/src/wdt.c#L51-L59)).
  So arming it takes three register writes. Nothing calls them with a long timeout.
- Payload variables are only `chosen.*`, `chainload=`, `display=`, `mitigations=`,
  `tso=` ([payload.c:195-206](https://github.com/AsahiLinux/m1n1/blob/c4c2fed8f894da5f41e59ae0d313621fa58804a1/src/payload.c#L195-L206)).
  None touches the watchdog. `chosen.*` only adds `/chosen` properties to the FDT stage 2
  builds, so it can't set `timeout-sec` on the watchdog node either. No build option
  mentions the watchdog: the Makefile only links `wdt.o`, and there's no `m1n1.conf` key.
- Chainload: stage 1 reads `m1n1/boot.bin`, appends the vars, a copy of SEPFW and
  boot_args, then runs a 9-instruction stub that copies the image over stage 1's own
  `_base` and branches to `_base + 0x800`
  ([chainload.c:24-109](https://github.com/AsahiLinux/m1n1/blob/c4c2fed8f894da5f41e59ae0d313621fa58804a1/src/chainload.c#L24-L109),
  [chainload_asm.S](https://github.com/AsahiLinux/m1n1/blob/c4c2fed8f894da5f41e59ae0d313621fa58804a1/src/chainload_asm.S)).
  Stage 2 then runs `_start` and `_start_c`, and its first console output is
  `uart_init()`, then `dockchannel_uart_init()`, then "Initializing"
  ([startup.c:194-202](https://github.com/AsahiLinux/m1n1/blob/c4c2fed8f894da5f41e59ae0d313621fa58804a1/src/startup.c#L194-L202)).

**So no stage-2 build, config or chainload variable can leave the watchdog armed.**
By the time stage 2 runs, stage 1 has already disabled it. Stage 2 disables it again
itself. Stage 2 could *re-arm* it only with a code change (for example, writing a
~170 s alarm just before `next_stage.entry(...)`). That covers stage 2's own tail and U-Boot's early init,
but **not** the stage 1 → stage 2 handoff where j1004 hung. Only a stage 1 that hands over
with the watchdog armed covers that, and stage 2 would then also have to not disable it.

### Replacing stage 1 is a human job (verified, installer source)

Stage 1 lives in the macOS stub's boot object, not on the ESP. The installer's only path
to change it is "m: Upgrade m1n1 on an existing OS". That path rebuilds the boot object
from the installer's own bundled `boot/m1n1.bin` and then runs step 2 again
([main.py:409-466](https://github.com/AsahiLinux/asahi-installer/blob/e93743d52178b3e26e428fa05174ef549abf30cb/src/main.py#L409-L466),
[main.py:1152](https://github.com/AsahiLinux/asahi-installer/blob/e93743d52178b3e26e428fa05174ef549abf30cb/src/main.py#L1152)).
Step 2 means booting into 1TR (hold power) and authenticating as the Mac's owner. Shipping
a *patched* stage 1 that way would also need a custom installer package (inferred).
maclab can't do any of this remotely. It's a supervised, one-time job per Volunteer Mac.

### U-Boot on t6030 takes it over and feeds it (source verified, hardware not)

- t6030's watchdog node is `compatible = "apple,t6030-wdt", "apple,t8103-wdt"` at
  `0x3642b0000`
  ([t6030.dtsi:487-493](https://github.com/torvalds/linux/blob/c339f4010dea8b0d754f84ace70cd1b089878ef8/arch/arm64/boot/dts/apple/t6030.dtsi#L487-L493)),
  the same address m1n1 printed. U-Boot's `apple_wdt` matches `apple,t8103-wdt`
  ([apple_wdt.c:80-83](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/drivers/watchdog/apple_wdt.c#L80-L83)).
  **Not checked:** that the DTB in m3pro's `boot.bin` (Omarchy's, not upstream) has
  the same fallback.
- `initr_watchdog()` probes every `UCLASS_WDT` device and starts it with
  `timeout-sec` or 60 s
  ([wdt-uclass.c:69-117](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/drivers/watchdog/wdt-uclass.c#L69-L117),
  [:231-255](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/drivers/watchdog/wdt-uclass.c#L231-L255);
  `WATCHDOG_AUTOSTART` default y, `WATCHDOG_TIMEOUT_MSECS` default 60000,
  [Kconfig:14-39](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/drivers/watchdog/Kconfig#L14-L39);
  `ARCH_APPLE` selects `SYSRESET_WATCHDOG(_AUTO)`,
  [arch/arm/Kconfig:1078-1079](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/arch/arm/Kconfig#L1078-L1079)).
- It runs late in `board_r`: after relocation, `initr_dm`, `initr_dm_devices` and serial
  ([board_r.c:630-663](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/common/board_r.c#L630-L663)).
  All of `board_f` and early `board_r` are in window A.
- It's fed by the cyclic framework from every `schedule()`. Its callers include
  `udelay()` ([time.c:195-199](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/lib/time.c#L195-L199)),
  the CLI line editor ([cli_readline.c:485](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/common/cli_readline.c#L485),
  [:564](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/common/cli_readline.c#L564)),
  and the EFI event loop that every boot-services wait runs
  ([efi_boottime.c:883](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/lib/efi_loader/efi_boottime.c#L883)).

**Not yet confirmed on any M3.** No m3pro capture of U-Boot's
`WDT:   Started watchdog@3642b0000 with servicing (60s timeout)` exists. The serial captures
labd keeps for m3pro start once Linux runs, and hardware-check §4's results table is empty.
This matters beyond this question. If U-Boot does *not* arm it on t6030, Linux's
`apple_wdt` finds it stopped. `watchdog.open_timeout` then does nothing, and a hang in Linux
before lab-agent opens `/dev/watchdog0` is never reset on an M3 either.

## 2. Does U-Boot or Limine feeding it while they wait defeat it? Yes.

### Verified (source)

- **U-Boot prompt.** `bootcmd` is `bootflow scan -b`
  ([apple_m1_defconfig:6](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/configs/apple_m1_defconfig#L6)).
  If nothing boots (or Limine exits back to it), `main_loop()` falls through to
  `cli_loop()` ([main.c:86](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/common/main.c#L86)),
  which feeds the watchdog while it waits for input forever. `BOOT_RETRY` /
  `RESET_TO_RETRY`, which would reset an idle prompt, are off by default
  ([boot/Kconfig:1755-1783](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/boot/Kconfig#L1755-L1783)).
  A U-Boot `panic()` does reset, because `PANIC_HANG` is off
  ([panic.c:21-33](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/lib/panic.c#L21-L33)).
- **UEFI's 5-minute watchdog doesn't help.** U-Boot arms the spec's 300 s EFI watchdog
  around `StartImage`, and on expiry it calls `do_reset`
  ([efi_helper.c:685](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/lib/efi_loader/efi_helper.c#L685),
  [efi_watchdog.c:25-31](https://github.com/AsahiLinux/u-boot/blob/b33034a515a068381c4aa28831fb709e91300cd1/lib/efi_loader/efi_watchdog.c#L25-L31)).
  But Limine turns it off as its first act: `SetWatchdogTimer(0, ...)`
  ([entry.s3.c:52](https://github.com/limine-bootloader/limine/blob/89a9512c623a4779cc73e3869383876d54264b28/common/entry.s3.c#L52)).
- **Limine waits through boot services**, `WaitForEvent` then `ReadKeyStroke`
  ([getchar.c:405](https://github.com/limine-bootloader/limine/blob/89a9512c623a4779cc73e3869383876d54264b28/common/lib/getchar.c#L405),
  [:498](https://github.com/limine-bootloader/limine/blob/89a9512c623a4779cc73e3869383876d54264b28/common/lib/getchar.c#L498)),
  so U-Boot feeds the hardware watchdog for as long as Limine waits.

### How a Limine error screen behaves

`panic(allow_menu, ...)`
([panic.s2.c:79-117](https://github.com/limine-bootloader/limine/blob/89a9512c623a4779cc73e3869383876d54264b28/common/lib/panic.s2.c#L79-L117)):

- `allow_menu = true` (most config and file errors, for example `efi: Failed to open
  image with path ... Is the path correct?`,
  [chainload.c:450](https://github.com/limine-bootloader/limine/blob/89a9512c623a4779cc73e3869383876d54264b28/common/protos/chainload.c#L450)):
  prints "Press a key to return to menu.", calls `getchar()`, **waits forever**, and is
  fed. It then re-enters the menu with `first_run = false`, which forces
  `skip_timeout = true`
  ([menu.c:2076-2077](https://github.com/limine-bootloader/limine/blob/89a9512c623a4779cc73e3869383876d54264b28/common/menu.c#L2076-L2077)).
  So the menu after a panic has **no timeout**, and nothing in this state ever resets.
- `allow_menu = false` (for example `efi: LoadImage failure`,
  [chainload.c:526](https://github.com/limine-bootloader/limine/blob/89a9512c623a4779cc73e3869383876d54264b28/common/protos/chainload.c#L526)):
  prints "System halted." and spins on `wfi` with no boot-services calls. Nothing feeds the
  watchdog, so U-Boot's 60 s watchdog resets the Mac (inferred). The one-shot was erased
  when Limine read it
  ([bli.c:242-256](https://github.com/limine-bootloader/limine/blob/89a9512c623a4779cc73e3869383876d54264b28/common/lib/bli.c#L242-L256)),
  so the Mac comes back on known-good.
- A default entry that doesn't resolve prints "Default entry is not valid, booting to
  menu." and shows the menu with no timeout
  ([menu.c:2080-2085](https://github.com/limine-bootloader/limine/blob/89a9512c623a4779cc73e3869383876d54264b28/common/menu.c#L2080-L2085)),
  which is fed forever.
- If the UKI's `StartImage` returns, Limine calls `Exit` back into U-Boot
  ([chainload.c:556-561](https://github.com/limine-bootloader/limine/blob/89a9512c623a4779cc73e3869383876d54264b28/common/protos/chainload.c#L556-L561)).
  U-Boot then tries the next bootflow and ends at its prompt (inferred from `bootflow scan -b`
  and `main_loop`).
- No `limine.conf` option reboots after a panic or puts a timeout on the post-panic menu
  (CONFIG.md has none). `hash_mismatch_panic` controls only whether a hash mismatch panics.

maclab's Lab entry is `protocol: efi` with a UKI path
([stage.go:180](../../internal/agent/stage.go)), so the panics above are the ones it can hit.

## 3. Known M3 (t6030) "Vectoring to next stage" hangs upstream

- **None found upstream.** Searched AsahiLinux/m1n1 issues and PRs, asahi-installer, u-boot,
  linux and docs for "Vectoring to next stage", "chainload" + hang, M3, t6030. Found
  nothing matching.
  Related but different: RVBAR handling in SMP and chainload on newer SoCs
  ([m1n1#536](https://github.com/AsahiLinux/m1n1/pull/536), reverted by
  [#576](https://github.com/AsahiLinux/m1n1/pull/576);
  [#585](https://github.com/AsahiLinux/m1n1/pull/585)), all about M4 (t8132).
- **A healthy M3 stage 2 prints right after "Vectoring".** A t8122 (MacBook Air M3) log
  in [chriskearney/aurora-linux#35](https://github.com/chriskearney/aurora-linux/issues/35)
  shows stage 1's "Vectoring to next stage..." followed at once by stage 2's
  "Initialized dockchannel UART at 0x2e4128000 / Initializing". On M3-family Macs the
  console is the dockchannel UART. Stage 2 added dockchannel as a fallback console only in
  [e3b42cbd](https://github.com/AsahiLinux/m1n1/commit/e3b42cbd) (2026-01-26).
- **j1004 on m3pro (verified, labd serial log).** Stage 1 is `m1n1 v1.6.1-dirty`, OS FW
  14.7 (iBoot-10151.140.19.700.2). It chainloaded `m1n1/boot.bin` (5 925 379 bytes, image
  0xb58000), printed "Vectoring to next stage..." at 10:22:42, then **nothing** for 2 min.
  After the OOB hard reset (macvdmtool) at 10:24:42, stage 1 ran again and **hung at
  exactly the same line** at 10:24:50. labd then asked a human to power cycle the Mac.
  So stage 2 never reached its UART init, **or** m3pro's stage 2 predates e3b42cbd and is
  silent on this console anyway. m3pro's stage-2 version isn't in any labd log, so this is
  open. (`/proc/device-tree/chosen/asahi,m1n1-stage2-version` on m3pro answers it.)
- **j1023 on m3pro:** the Mac didn't check in within 4 min of an ordinary reboot to its
  known-good kernel. **No serial log was captured**, so where it hung is unknown. Calling it
  "the same hang" is a guess.
- **josip-omarchy j1028:** no serial and no OOB. It didn't check in within 6 min, nor in the
  47 min after. Where it hung (pre-Linux, or in Linux before lab-agent ran) can't be told from
  labd's data.

The one point j1004 does show: **a full reset did not clear that hang.** A watchdog reset in
window A would most likely have ended the same way, as a reboot loop rather than a
recovery. That is inferred: a watchdog reset and a macvdmtool reset weren't compared. A
human power cycle evidently did clear it, because m3pro has passed many jobs since.

## 4. Proposal

Treat this as **not possible today**: no setting makes a Lab boot recover by itself from a
pre-Linux hang. The steps below are ordered by cost.

1. **Confirm the watchdog works at all on t6030** (Lab-owned m3pro, which has OOB and
   serial). Run hardware-check §4.1-4.2: look for U-Boot's `WDT: Started` line, and boot
   with `watchdog.handle_boot_enabled=0`, expecting a reset ~30 s after `apple_wdt` probes.
   Also record m3pro's stage-2 version. If U-Boot doesn't arm it on M3, the Linux-phase
   tier (`watchdog.open_timeout`) is missing there too. That would be a bigger and more
   fixable gap than anything in this document, and it could explain josip-omarchy.
2. **Don't cause Limine's waits.** These are the window C hangs maclab can prevent. Before
   arming the one-shot, lab-agent should check that the staged UKI path exists on the ESP and
   reads back intact, that `limine.conf` still names the entry (limine-snapper-sync rewrites
   `limine.conf` on Omarchy), and that `default_entry` resolves. It should never add a
   `#hash` suffix to the Lab entry, because a mismatch is a `panic(true)` that waits forever.
   Some of this may already exist; it wasn't audited here.
3. **Gate Volunteer Lab boots on the boot chain, not just the kernel.** Windows A and C don't
   depend on the test kernel. They depend on stage 1, `boot.bin` (stage 2 + U-Boot + DTB),
   Limine and `limine.conf`. Before a Volunteer Mac's first Lab boot, and after any change to
   those, require the same stage-1/stage-2/U-Boot/Limine versions to have done N clean
   reboots on a Lab-owned Mac of the same SoC (m3pro for t6030). This is the only lever here
   that needs no upstream change. It reduces the risk; it doesn't remove it. j1023 suggests
   m3pro can hang on an ordinary reboot.
4. **Long term (upstream; per-Mac human step for stage 1):**
   - m1n1: an opt-in variable (say `wdt_handoff=<s>`, at most 178) that arms the primary
     watchdog with that timeout just before `next_stage.entry(...)`, and makes the next
     stage's `wdt_disable()` re-arm it instead of disabling it. With that variable set in
     **stage 1** (installed once per Mac through asahi-installer's upgrade + 1TR), window A
     is covered up to U-Boot's `initr_watchdog()`, which reprograms it to 60 s.
   - Limine: a config option to reboot after a panic (or to give the post-panic menu a
     timeout). Or, for the U-Boot prompt only, a uboot-asahi build with `BOOT_RETRY` +
     `RESET_TO_RETRY`. Building U-Boot with `CONFIG_WATCHDOG=n` ("start but never
     service") would also cover every Limine wait. But it would reset *every* boot that sits
     at the Limine menu for 60 s, and `boot.bin` can't tell a Lab boot from a normal one, so
     it isn't suitable for a Volunteer's daily boot.
   - Even with all of the above, a hang that survives a SoC reset, as j1004's did, still
     needs a human.

## How to check the open points (no code changes)

- m3pro stage-2 version: `cat /proc/device-tree/chosen/asahi,m1n1-stage2-version` (needs
  `lab exec`, not done here).
- U-Boot arming on t6030: hardware-check §4.1 (serial during U-Boot) and §4.2.
- Whether a watchdog reset clears a j1004-type hang: not testable on demand. Next time one
  happens on m3pro, let the 2 min pass with a watchdog armed (needs the m1n1 patch), or
  compare macvdmtool's reset with a power-button hold.
