package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDocker is a docker CLI that logs each call and keeps one container's
// /io in a directory: cp copies in and out of it, and start runs a pretend
// kbuild there, which fails when the config says "fail".
const fakeDocker = `#!/bin/sh
state=$(dirname "$0")/state
echo "$*" >> "$state/calls"
[ "$1" = --context ] && shift 2
case "$1" in
create) mkdir -p "$state/io" ;;
cp)
  case "$2" in
  *:/io/.) cp -R "$state/io/." "$3" ;;
  *) cp -R "$2" "$state/io-in" && cp -R "$state/io-in/." "$state/io" ;;
  esac ;;
start)
  echo "kbuild: building"
  grep -q fail "$state/io/config" && { echo "kbuild: failed" >&2; exit 3; }
  echo 7.2.0-lab-x > "$state/io/release"
  echo kernel > "$state/io/kernel.tar.zst" ;;
esac
`

func dockerBuilder(t *testing.T) (*builder, string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.Mkdir(filepath.Join(bin, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(bin, "docker")
	if err := os.WriteFile(p, []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	return &builder{runtime: "docker", docker: p, dockerContext: "colima", cpus: "12", mem: "24G"}, filepath.Join(bin, "state")
}

func calls(t *testing.T, state string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// With --runtime docker, /io is copied into the container and back out, never
// bind-mounted: Colima shares only $HOME into its VM, and a bind mount of
// anything else is silently empty (malik-na/maclab#45).
func TestDockerBuildCopiesIOInAndOut(t *testing.T) {
	b, state := dockerBuilder(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config"), []byte("CONFIG_ARM64=y\n"), 0o644)
	var out bytes.Buffer
	if err := b.buildVM(context.Background(), "maclab-b1", "maclab-kbuild:1", "maclab-cache", dir, []string{"URL=u", "SHA=s"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if rel, _ := os.ReadFile(filepath.Join(dir, "release")); strings.TrimSpace(string(rel)) != "7.2.0-lab-x" {
		t.Fatalf("release %q not copied out", rel)
	}
	if _, err := os.Stat(filepath.Join(dir, "kernel.tar.zst")); err != nil {
		t.Fatal("artifact not copied out:", err)
	}
	if !strings.Contains(out.String(), "kbuild: building") {
		t.Fatalf("build output not streamed: %q", out.String())
	}
	want := []string{
		"--context colima create --name maclab-b1 --cpus 12 --memory 24G -v maclab-cache:/cache -e URL=u -e SHA=s maclab-kbuild:1",
		"--context colima cp " + dir + "/. maclab-b1:/io",
		"--context colima start --attach maclab-b1",
		"--context colima cp maclab-b1:/io/. " + dir,
		"--context colima rm --force maclab-b1",
	}
	if got := calls(t, state); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("docker calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A kbuild that fails is kbuild's error, and the container is removed anyway.
func TestDockerBuildFailureRemovesTheContainer(t *testing.T) {
	b, state := dockerBuilder(t)
	b.dockerContext = ""
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config"), []byte("fail\n"), 0o644)
	var out bytes.Buffer
	err := b.buildVM(context.Background(), "maclab-b2", "maclab-kbuild:1", "maclab-cache", dir, nil, &out)
	if !errors.As(err, new(*exec.ExitError)) {
		t.Fatalf("err %v, want kbuild's exit status", err)
	}
	if !strings.Contains(out.String(), "kbuild: failed") {
		t.Fatalf("kbuild's error not streamed: %q", out.String())
	}
	got := calls(t, state)
	if last := got[len(got)-1]; last != "rm --force maclab-b2" {
		t.Fatalf("last docker call %q, want the container removed", last)
	}
	for _, c := range got {
		if strings.HasPrefix(c, "cp maclab-b2:") {
			t.Fatalf("copied the outputs of a failed build: %q", c)
		}
	}
}
