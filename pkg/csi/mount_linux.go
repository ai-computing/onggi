//go:build linux

package csi

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/unix"
)

// NewMounter returns the Linux mounter: bind mounts through mount(2).
func NewMounter() Mounter { return &linuxMounter{} }

type linuxMounter struct {
	mu sync.Mutex // serializes the read-only self-binds of sources
}

const roFlags = unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV

// BindReadOnly makes source a read-only mount first (a bind onto itself, in the plugin's own
// mount namespace), then binds it at target. A bind copies the flags of the mount it binds, and
// so do the copies that propagation creates in the host's namespace. Remounting only the target
// read-only would not do: a remount does not propagate, so the host would keep a writable mount
// (seen on the lab cluster, #27).
func (m *linuxMounter) BindReadOnly(source, target string) error {
	m.mu.Lock()
	ok, err := isMountedIn("/proc/self/mountinfo", source)
	if err == nil && !ok {
		if err = unix.Mount(source, source, "", unix.MS_BIND, ""); err == nil {
			if err = unix.Mount("", source, "", roFlags, ""); err != nil {
				unix.Unmount(source, 0)
			}
		}
	}
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf("read-only bind of %s: %w", source, err)
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s on %s: %w", source, target, err)
	}
	// The new mount is read-only already; this also covers a source made writable meanwhile.
	if err := unix.Mount("", target, "", roFlags, ""); err != nil {
		unix.Unmount(target, 0)
		return fmt.Errorf("remount %s read-only: %w", target, err)
	}
	return nil
}

func (m *linuxMounter) Unmount(target string) error {
	err := unix.Unmount(target, 0)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) { // not mounted, or gone
		return nil
	}
	if err != nil {
		return fmt.Errorf("unmount %s: %w", target, err)
	}
	return nil
}

func (m *linuxMounter) IsMounted(target string) (bool, error) {
	return isMountedIn("/proc/self/mountinfo", target)
}
