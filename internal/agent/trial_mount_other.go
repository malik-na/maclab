//go:build !linux

package agent

import "errors"

// Trial systems are btrfs snapshots on a Linux Mac; elsewhere (labd and lab
// build this package on macOS) there is no top level to mount.
func mountTop(dev, dir string) error { return errors.New("Trial systems need Linux") }

func unmountTop(dir string) error { return nil }
