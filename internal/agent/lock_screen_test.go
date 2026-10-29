package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iconidentify/maclab/internal/api"
)

const lockScreenBase = "root=UUID=abc rw quiet"

func labBootLine() string { return lockScreenBase + " " + strings.Join(labBootParams(0), " ") }

func TestLabBootOnAVolunteersMacGetsAnAutologinDropIn(t *testing.T) {
	d, ok := labBootAutologin(labBootLine(), api.KindVolunteer, "ada", false)
	if !ok {
		t.Fatal("no autologin drop-in on a Lab boot of a Volunteer's Mac")
	}
	if !strings.Contains(d.Content, "[Autologin]") {
		t.Errorf("drop-in has no [Autologin] section:\n%s", d.Content)
	}
}

func TestAutologinDropInLogsInTheVolunteersGuiUser(t *testing.T) {
	d, ok := labBootAutologin(labBootLine(), api.KindVolunteer, "ada", false)
	if !ok {
		t.Fatal("no drop-in")
	}
	var users []string
	for _, l := range strings.Split(d.Content, "\n") {
		if u, ok := strings.CutPrefix(strings.TrimSpace(l), "User="); ok {
			users = append(users, u)
		}
	}
	if len(users) != 1 || users[0] != "ada" {
		t.Errorf("User= lines %v, want exactly [ada]:\n%s", users, d.Content)
	}
}

func TestAutologinDropInLivesUnderRunNeverEtc(t *testing.T) {
	d, ok := labBootAutologin(labBootLine(), api.KindVolunteer, "ada", false)
	if !ok {
		t.Fatal("no drop-in")
	}
	if !strings.HasPrefix(d.Path, "/run/") || strings.HasPrefix(d.Path, "/etc/") || !strings.HasSuffix(d.Path, ".conf") {
		t.Errorf("drop-in path %q: want a .conf under /run, so it never outlives the boot", d.Path)
	}
}

func TestBootWithoutTheLabBootMarkerCannotAutologin(t *testing.T) {
	for name, cmdline := range map[string]string{
		"the Volunteer's own default entry": lockScreenBase,
		"empty command line":                "",
		"a Lab boot's other params only":    lockScreenBase + " panic=10 watchdog.open_timeout=150",
		"marker with another value":         lockScreenBase + " maclab.lab_boot=0",
		"marker as a prefix":                lockScreenBase + " maclab.lab_boot=10",
		"marker as a suffix":                lockScreenBase + " xmaclab.lab_boot=1",
		"marker inside a longer token":      lockScreenBase + " foo=maclab.lab_boot=1",
		"marker name without value":         lockScreenBase + " maclab.lab_boot",
	} {
		if d, ok := labBootAutologin(cmdline, api.KindVolunteer, "ada", false); ok {
			t.Errorf("%s: drop-in written: %+v", name, d)
		}
	}
}

func TestLabBootDropInIsDecidedFromTheCmdlineAlone(t *testing.T) {
	// the marker is where the decision comes from, wherever it sits on the line
	for _, cmdline := range []string{"maclab.lab_boot=1", "maclab.lab_boot=1 " + lockScreenBase, lockScreenBase + " maclab.lab_boot=1  quiet"} {
		if !IsLabBoot(cmdline) {
			t.Fatalf("IsLabBoot(%q) is false", cmdline)
		}
		if _, ok := labBootAutologin(cmdline, api.KindVolunteer, "ada", false); !ok {
			t.Errorf("no drop-in for Lab boot %q", cmdline)
		}
	}
}

func TestVolunteerMacWithoutAGuiUserGetsNoAutologinDropIn(t *testing.T) {
	if d, ok := labBootAutologin(labBootLine(), api.KindVolunteer, "", false); ok {
		t.Errorf("drop-in with no user to log in: %+v", d)
	}
}

func TestEncryptedMacKeepsOmarchysOwnAutologin(t *testing.T) {
	if d, ok := labBootAutologin(labBootLine(), api.KindVolunteer, "ada", true); ok {
		t.Errorf("drop-in on an encrypted Mac, where the installer's autologin stands: %+v", d)
	}
}

func TestLabOwnedMacNeedsNoDropInBecauseItKeepsPermanentAutologin(t *testing.T) {
	if d, ok := labBootAutologin(labBootLine(), api.KindLabOwned, "ada", false); ok {
		t.Errorf("drop-in on a Lab-owned Mac: %+v", d)
	}
}

// SDDM 0.21 reads only /etc/sddm.conf(.d) and /usr/lib/sddm/sddm.conf.d: on
// josip-omarchy it ignored the drop-in under /run and showed its greeter.
func TestSDDMsCopyIsWhereSDDMReadsAfterOmarchysOwn(t *testing.T) {
	if filepath.Dir(labBootSDDMPath) != "/etc/sddm.conf.d" {
		t.Errorf("SDDM's copy %s is not in /etc/sddm.conf.d", labBootSDDMPath)
	}
	if filepath.Base(labBootSDDMPath) <= "99-omarchy-login.conf" {
		t.Errorf("%s sorts before Omarchy's 99-omarchy-login.conf, which would override it", labBootSDDMPath)
	}
}

func quiet(string, ...any) {}

func TestLabBootWritesTheDropInAndSDDMsCopy(t *testing.T) {
	root := t.TempDir()
	if err := placeLabBootAutologin(root, labBootLine(), api.KindVolunteer, "ada", func() bool { return false }, quiet); err != nil {
		t.Fatal(err)
	}
	run, err := os.ReadFile(filepath.Join(root, labBootAutologinPath))
	if err != nil {
		t.Fatalf("no drop-in under /run: %v", err)
	}
	etc, err := os.ReadFile(filepath.Join(root, labBootSDDMPath))
	if err != nil {
		t.Fatalf("no copy for SDDM: %v", err)
	}
	if string(etc) != string(run) || !strings.Contains(string(etc), "User=ada") {
		t.Errorf("SDDM's copy:\n%s\nthe drop-in:\n%s", etc, run)
	}
}

// A Lab boot that went down before its session came up leaves SDDM's copy.
// Any later boot removes it before SDDM starts, and only it.
func TestEveryBootRemovesALeftoverCopyBeforeSDDM(t *testing.T) {
	for name, c := range map[string]struct {
		cmdline   string
		encrypted bool
	}{
		"the Volunteer's own boot":    {lockScreenBase, false},
		"an encrypted Mac's Lab boot": {labBootLine(), true},
	} {
		root := fakeRoot(t, map[string]string{
			labBootSDDMPath[1:]:                        "[Autologin]\nUser=ada\n",
			"etc/sddm.conf.d/99-omarchy-login.conf":    "[Users]\nRememberLastUser=true\n",
			"etc/sddm.conf.d/zz-maclab-autologin.conf": "[Autologin]\nUser=lab\n",
		})
		encrypted := func() bool {
			if !IsLabBoot(c.cmdline) {
				t.Errorf("%s: asked whether the disk is encrypted on a boot that is not a Lab boot", name)
			}
			return c.encrypted
		}
		if err := placeLabBootAutologin(root, c.cmdline, api.KindVolunteer, "ada", encrypted, quiet); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, p := range []string{labBootSDDMPath, labBootAutologinPath} {
			if _, err := os.Stat(filepath.Join(root, p)); err == nil {
				t.Errorf("%s: %s is there", name, p)
			}
		}
		for _, p := range []string{"etc/sddm.conf.d/99-omarchy-login.conf", "etc/sddm.conf.d/zz-maclab-autologin.conf"} {
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				t.Errorf("%s: %s, not the lab boot's, was removed", name, p)
			}
		}
	}
}

func TestLockUsesOmarchysQuickshellLockWhenItIsThere(t *testing.T) {
	argv, ok := lockCommand(func(n string) bool { return n == "omarchy-system-lock" || n == "hyprlock" })
	if !ok || len(argv) == 0 || argv[0] != "omarchy-system-lock" {
		t.Errorf("lock command %v (ok=%v), want omarchy-system-lock", argv, ok)
	}
	argv, ok = lockCommand(func(n string) bool { return n == "omarchy-system-lock" })
	if !ok || len(argv) == 0 || argv[0] != "omarchy-system-lock" {
		t.Errorf("lock command %v (ok=%v), want omarchy-system-lock", argv, ok)
	}
}

func TestLockFallsBackToHyprlockOnOlderOmarchy(t *testing.T) {
	argv, ok := lockCommand(func(n string) bool { return n == "hyprlock" })
	if !ok || len(argv) == 0 || argv[0] != "hyprlock" {
		t.Errorf("lock command %v (ok=%v), want hyprlock", argv, ok)
	}
}

func TestNoLockCommandMeansNoLockCommand(t *testing.T) {
	if argv, ok := lockCommand(func(string) bool { return false }); ok {
		t.Errorf("lock command %v with neither lock installed", argv)
	}
}
