//go:build !linux

package csi

import "errors"

// NewMounter returns a mounter that fails: the node plugin mounts on Linux only.
func NewMounter() Mounter { return unsupported{} }

type unsupported struct{}

var errUnsupported = errors.New("csi: mounts are supported on Linux only")

func (unsupported) BindReadOnly(string, string) error { return errUnsupported }
func (unsupported) Unmount(string) error              { return errUnsupported }
func (unsupported) IsMounted(string) (bool, error)    { return false, errUnsupported }
