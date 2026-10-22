package csi

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Mounter is what the node service needs from the OS. NewMounter returns the Linux one; tests
// use a fake.
type Mounter interface {
	// BindReadOnly bind-mounts source at target (an existing directory) read-only.
	BindReadOnly(source, target string) error
	// Unmount unmounts target; a target that is not mounted is not an error.
	Unmount(target string) error
	// IsMounted reports whether target is a mount point.
	IsMounted(target string) (bool, error)
}

// mountPoints reads mount points from a mountinfo file (proc(5)): the fifth field, with the
// octal escapes the kernel uses for spaces and the like undone.
func mountPoints(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		out = append(out, unescapeOctal(f[4]))
	}
	return out, sc.Err()
}

func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// isMountedIn reports whether target is among the mount points listed in the mountinfo file.
func isMountedIn(mountinfo, target string) (bool, error) {
	f, err := os.Open(mountinfo)
	if err != nil {
		return false, err
	}
	defer f.Close()
	pts, err := mountPoints(f)
	if err != nil {
		return false, err
	}
	target = filepath.Clean(target)
	for _, p := range pts {
		if p == target {
			return true, nil
		}
	}
	return false, nil
}
