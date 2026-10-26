package server

import (
	"strings"
	"testing"
	"time"

	"github.com/iconidentify/maclab/internal/agent"
	"github.com/iconidentify/maclab/internal/api"
)

func TestVolunteerLabRunsVolunteerCommands(t *testing.T) {
	l := newLabWith(t, labConfig{volunteer: true})
	l.waitDevice(func(d api.Device) bool { return d.BootID != "" })
	if _, err := l.volunteer("no-such-command"); err == nil || !strings.Contains(err.Error(), `"no-such-command"`) {
		t.Fatalf("an unknown Volunteer command: %v", err)
	}
}

func TestEncryptedMacWaitsForItsPassword(t *testing.T) {
	l := newLabWith(t, labConfig{volunteer: true})
	l.sim.Encrypted = true
	before := l.waitDevice(func(d api.Device) bool { return d.BootID != "" })
	l.sim.PowerCycle()
	time.Sleep(time.Second) // several reboot delays
	if d := l.s.dev(l.name).snapshot(); d.BootID != before.BootID {
		t.Fatal("an encrypted Mac booted without its disk password")
	}
	l.sim.TypePassword()
	l.waitDevice(func(d api.Device) bool { return d.BootID != "" && d.BootID != before.BootID })
}

func TestRollbackReturnsTheMacToItsSnapshot(t *testing.T) {
	l := newLabWith(t, labConfig{volunteer: true})
	l.waitDevice(func(d api.Device) bool { return d.BootID != "" })
	l.sim.Snapshot()
	l.sim.DefaultKernel = "7.3-installed" // what an installer leaves behind
	l.sim.PowerCycle()
	l.waitDevice(func(d api.Device) bool { return d.Kernel == "7.3-installed" })
	if err := l.sim.Rollback(); err != nil {
		t.Fatal(err)
	}
	l.sim.PowerCycle()
	l.waitDevice(func(d api.Device) bool { return d.Kernel == agent.SimKnownGood })
}

func TestLabBootThatLosesTheNetworkNeverChecksIn(t *testing.T) {
	l := newLabWith(t, labConfig{volunteer: true, oob: true})
	l.openForTheLab(time.Now())
	l.baseline()
	j := l.run(api.JobSpec{Kernel: l.artifact("krel=7.2-offline behavior=no-network")})
	if j.Result.Booted || j.Outcome == api.OutcomePass {
		t.Fatalf("a Lab boot with no network reached labd: %s %+v", j.Outcome, j.Result)
	}
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.Kernel == agent.SimKnownGood })
}
