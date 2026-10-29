package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iconidentify/maclab/internal/api"
)

// A Volunteer's Mac never loses its lock screen to the lab. Tests need a
// logged-in Wayland session for the GUI user, not an unlocked screen, so a
// Lab boot logs the GUI user in and locks the session at once. The autologin
// is decided per boot from the command line and recorded under /run. SDDM
// reads only /etc/sddm.conf(.d) and /usr/lib/sddm/sddm.conf.d, so a copy goes
// in /etc/sddm.conf.d for SDDM to read, and is removed once the session is up
// and before SDDM starts on every boot: any boot of the Volunteer's own entry
// (a deadline fallback, a power cut) can't have it.

// AutologinDropIn is the SDDM drop-in a Lab boot gets.
type AutologinDropIn struct {
	Path    string // under /run, so it never outlives the boot
	Content string
}

const labBootAutologinPath = "/run/sddm.conf.d/zz-maclab-lab-boot-autologin.conf"

// labBootSDDMPath is the copy of the drop-in that SDDM reads. It sorts after
// Omarchy's own 99-omarchy-login.conf.
const labBootSDDMPath = "/etc/sddm.conf.d/zz-maclab-lab-boot-autologin.conf"

// labBootAutologin is the drop-in a boot needs, if any: only a Lab boot of a
// Volunteer's unencrypted Mac with a GUI user. A Lab-owned Mac keeps its
// permanent autologin, and an encrypted Mac keeps the one Omarchy's installer
// set (the disk password prompt is its auth boundary).
func labBootAutologin(cmdline, kind, guiUser string, encrypted bool) (AutologinDropIn, bool) {
	if !IsLabBoot(cmdline) || kind != api.KindVolunteer || guiUser == "" || encrypted {
		return AutologinDropIn{}, false
	}
	return AutologinDropIn{Path: labBootAutologinPath, Content: fmt.Sprintf(
		"# maclab: this is a Lab boot, so the GUI user is logged in (and locked at once). Gone with the boot.\n[Autologin]\nUser=%s\nSession=%s\nRelogin=false\n",
		guiUser, desktopSession())}, true
}

// lockCommand is Omarchy's own lock: Quickshell's omarchy-system-lock on
// current Omarchy, hyprlock on older ones.
func lockCommand(has func(name string) bool) ([]string, bool) {
	for _, name := range []string{"omarchy-system-lock", "hyprlock"} {
		if has(name) {
			return []string{name}, true
		}
	}
	return nil, false
}

const (
	labBootSessionUnitName = "maclab-lab-boot-session.service"
	labBootSessionUnitPath = "/etc/systemd/system/" + labBootSessionUnitName
	labBootLockUnitName    = "maclab-lab-boot-lock.service"
	labBootLockUnitPath    = "/etc/systemd/system/" + labBootLockUnitName
)

// labBootSessionUnit writes the autologin drop-in, before SDDM starts.
func labBootSessionUnit() string {
	return `[Unit]
Description=maclab: log the GUI user in on a Lab boot of a Volunteer's Mac
DefaultDependencies=no
After=local-fs.target
Before=sddm.service display-manager.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=` + binPath + ` lab-boot-session

[Install]
WantedBy=sysinit.target
`
}

// labBootLockUnit locks the session the drop-in logged in.
func labBootLockUnit() string {
	return `[Unit]
Description=maclab: lock the GUI user's session on a Lab boot of a Volunteer's Mac
After=display-manager.service

[Service]
ExecStart=` + binPath + ` lab-boot-lock

[Install]
WantedBy=graphical.target
`
}

func desktopSession() string {
	if b, err := os.ReadFile("/var/lib/sddm/state.conf"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "Session="); ok && v != "" {
				return filepath.Base(strings.TrimSpace(v))
			}
		}
	}
	return "hyprland-uwsm.desktop"
}

// rootEncrypted is whether the root filesystem sits on a LUKS device. When it
// can't tell, it says yes: the lab then adds no autologin.
func rootEncrypted() bool {
	src, err := exec.Command("findmnt", "-no", "SOURCE", "/").Output()
	if err != nil {
		return true
	}
	dev, _, _ := strings.Cut(strings.TrimSpace(string(src)), "[")
	out, err := exec.Command("lsblk", "-srno", "TYPE", dev).Output()
	return err != nil || strings.Contains(string(out), "crypt")
}

// WriteLabBootAutologin is the unit before SDDM, on every boot: it removes a
// copy for SDDM that an earlier Lab boot left, and on a Lab boot of a
// Volunteer's Mac writes the autologin drop-in under /run and its copy for SDDM.
func WriteLabBootAutologin(cfg *Config, say func(string, ...any)) error {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return err
	}
	return placeLabBootAutologin("/", string(b), cfg.Enrollment.Kind, cfg.GUIUser, rootEncrypted, say)
}

// placeLabBootAutologin is WriteLabBootAutologin under root ("/" except in
// tests). encrypted is asked only on a Lab boot.
func placeLabBootAutologin(root, cmdline, kind, guiUser string, encrypted func() bool, say func(string, ...any)) error {
	// A Lab boot that went down before its session came up leaves its copy;
	// it must not log the Volunteer in on this boot unless this is a Lab boot.
	switch err := os.Remove(filepath.Join(root, labBootSDDMPath)); {
	case err == nil:
		say("removed %s, left by an earlier Lab boot", labBootSDDMPath)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if !IsLabBoot(cmdline) {
		return nil
	}
	d, ok := labBootAutologin(cmdline, kind, guiUser, encrypted())
	if !ok {
		say("Lab boot with no autologin drop-in (no GUI user, or an encrypted Mac keeping Omarchy's own)")
		return nil
	}
	for _, p := range []string{d.Path, labBootSDDMPath} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(d.Content), 0o644); err != nil {
			return err
		}
	}
	say("Lab boot: %s logs in as %s, locked at once", labBootSDDMPath, guiUser)
	return nil
}

// labBootLockedPath is the lock unit's note of how locking went: "locked", or
// "failed: <why>". The tests of a job wait for it.
const labBootLockedPath = "/run/maclab-lab-boot-lock"

// lockHoldTime is how long the lock process must stay up to count as locked.
const lockHoldTime = 5 * time.Second

// LockLabBootSession locks the GUI user's session once the drop-in has logged
// it in, and notes the result in labBootLockedPath. A session it can't lock is
// terminated: a Lab boot never leaves a Volunteer's session unlocked. A boot
// without the drop-in has nothing to lock.
func LockLabBootSession(cfg *Config, say func(string, ...any)) error {
	if _, err := os.Stat(labBootAutologinPath); err != nil {
		return nil
	}
	err := lockSession(cfg.GUIUser, say)
	if err == nil {
		return nil // the lock was lifted by the Volunteer
	}
	os.Remove(labBootSDDMPath) // gone already if the session came up
	say("%v: terminating the session", err)
	os.WriteFile(labBootLockedPath, []byte("failed: "+err.Error()), 0o644)
	exec.Command("loginctl", "terminate-user", cfg.GUIUser).Run()
	return err
}

// lockSession runs the lock as user and returns when the Volunteer unlocks, or
// with an error if the lock did not come up.
func lockSession(user string, say func(string, ...any)) error {
	argv, ok := lockCommand(func(n string) bool { _, err := exec.LookPath(n); return err == nil })
	if !ok {
		return fmt.Errorf("neither omarchy-system-lock nor hyprlock is installed")
	}
	u, genv, err := guiEnv(user, 2*time.Minute)
	// SDDM has logged the session in, or won't on this boot: its copy goes now.
	os.Remove(labBootSDDMPath)
	if err != nil {
		return err
	}
	asUser := func(argv ...string) *exec.Cmd {
		return exec.Command("runuser", append([]string{"-u", u.Username, "--", "env"}, append(genv, argv...)...)...)
	}
	cmd := asUser(argv...)
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		// Omarchy 4.5's omarchy-system-lock asks omarchy-shell for the lock and
		// returns, 0 even when the shell didn't take the request.
		if _, shell := exec.LookPath("omarchy-shell"); err != nil || shell != nil {
			return fmt.Errorf("%s exited at once: %v", argv[0], err)
		}
		sh := shellLock{
			Status: func() ([]byte, error) { return asUser("omarchy-shell", "lock", "status").Output() },
			Lock:   func() error { return asUser(argv...).Run() },
		}
		if err := sh.await(shellLockWait, 2*time.Second); err != nil {
			return err
		}
		say("locked %s's session with %s (omarchy-shell)", u.Username, argv[0])
		os.WriteFile(labBootLockedPath, []byte("locked"), 0o644)
		sh.awaitUnlock(10 * time.Second)
		return nil
	case <-time.After(lockHoldTime):
	}
	say("locked %s's session with %s", u.Username, argv[0])
	os.WriteFile(labBootLockedPath, []byte("locked"), 0o644)
	<-exited // the lock stays up until the Volunteer unlocks
	return nil
}

// shellLockWait bounds how long omarchy-shell has to come up and lock: it
// starts after the compositor, so the first requests can go unanswered.
const shellLockWait = 90 * time.Second

// shellLock is a lock held by omarchy-shell rather than by a lock process.
type shellLock struct {
	Status func() ([]byte, error) // omarchy-shell lock status
	Lock   func() error           // asks for the lock again
}

// shellLockStatus is the part of omarchy-shell's lock status the lab reads.
// Secure means the compositor confirmed the session lock: only then is the
// screen covered.
type shellLockStatus struct {
	Locked    bool `json:"locked"`
	Requested bool `json:"requested"`
	Secure    bool `json:"secure"`
}

func (s shellLock) status() (shellLockStatus, error) {
	var st shellLockStatus
	out, err := s.Status()
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(bytes.TrimSpace(out), &st)
}

// await returns once the lock is secure, asking for it again while the shell
// has no request (it wasn't up yet), or an error after wait.
func (s shellLock) await(wait, poll time.Duration) error {
	deadline := time.Now().Add(wait)
	var last error
	for {
		st, err := s.status()
		switch {
		case err == nil && st.Secure:
			return nil
		case err == nil && !st.Requested:
			last = errors.New("omarchy-shell has no lock request")
			s.Lock()
		case err == nil:
			last = errors.New("omarchy-shell's lock is not secure yet")
		default:
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("omarchy-shell did not lock the session in %s: %v", wait, last)
		}
		time.Sleep(poll)
	}
}

// awaitUnlock returns once the Volunteer has unlocked. A shell that stops
// answering leaves the compositor's lock up, so it counts as locked.
func (s shellLock) awaitUnlock(poll time.Duration) {
	for {
		if st, err := s.status(); err == nil && !st.Locked {
			return
		}
		time.Sleep(poll)
	}
}

// labBootLockWait bounds how long a job's tests wait for the lock.
const labBootLockWait = 3 * time.Minute

// awaitLabBootLock holds a job's tests back on a Lab boot that logged the GUI
// user in, until the session is locked. If the lock can't be confirmed in
// time, the session is terminated and the job fails.
func (l *Linux) awaitLabBootLock(ctx context.Context) error {
	if _, err := os.Stat(labBootAutologinPath); err != nil {
		return nil
	}
	deadline := time.Now().Add(labBootLockWait)
	for {
		b, _ := os.ReadFile(labBootLockedPath)
		switch got := string(b); {
		case got == "locked":
			return nil
		case strings.HasPrefix(got, "failed"):
			return fmt.Errorf("the Volunteer's session could not be locked (%s); no test ran on it", got)
		case time.Now().After(deadline):
			exec.Command("loginctl", "terminate-user", l.GUIUser).Run()
			return fmt.Errorf("the Volunteer's session was not locked within %s; it was terminated and no test ran on it", labBootLockWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
