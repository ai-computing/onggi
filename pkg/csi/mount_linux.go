//go:build linux

package csi

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// NewMounter returns the Linux mounter: bind mounts through mount(2).
func NewMounter() Mounter { return linuxMounter{} }

type linuxMounter struct{}

func (linuxMounter) BindReadOnly(source, target string) error {
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s on %s: %w", source, target, err)
	}
	// A bind mount takes the source's flags; read-only needs a remount of the new mount point.
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV)
	if err := unix.Mount("", target, "", flags, ""); err != nil {
		unix.Unmount(target, 0)
		return fmt.Errorf("remount %s read-only: %w", target, err)
	}
	return nil
}

func (linuxMounter) Unmount(target string) error {
	err := unix.Unmount(target, 0)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) { // not mounted, or gone
		return nil
	}
	if err != nil {
		return fmt.Errorf("unmount %s: %w", target, err)
	}
	return nil
}

func (linuxMounter) IsMounted(target string) (bool, error) {
	return isMountedIn("/proc/self/mountinfo", target)
}
