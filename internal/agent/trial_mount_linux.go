//go:build linux

package agent

import "golang.org/x/sys/unix"

// mountTop mounts the top level (subvolid=5) of btrfs device dev at dir.
func mountTop(dev, dir string) error {
	return unix.Mount(dev, dir, "btrfs", unix.MS_NOATIME, "subvolid=5")
}

func unmountTop(dir string) error { return unix.Unmount(dir, 0) }
