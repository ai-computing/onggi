//go:build linux

package csi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
)

// Run only in a disposable mount namespace with CAP_SYS_ADMIN, never against host paths.
func TestLinuxMountLifecycle(t *testing.T) {
	if os.Getenv("ONGGI_TEST_MOUNTS") != "1" {
		t.Skip("set ONGGI_TEST_MOUNTS=1 in a disposable mount namespace with CAP_SYS_ADMIN")
	}
	n, f, pulls, _ := newNode(t)
	n.Mounter = NewMounter()
	dir := n.M.Dir(f.a)
	pods := t.TempDir()
	targets := []string{filepath.Join(pods, "pod-1", "mount"), filepath.Join(pods, "pod-2", "mount")}
	paths := []string{targets[0], targets[1], dir}
	t.Cleanup(func() {
		// Targets must leave before their shared read-only source and TempDir cleanup.
		for _, p := range paths {
			mounted, err := n.Mounter.IsMounted(p)
			if err != nil {
				t.Errorf("cleanup check %s: %v", p, err)
				continue
			}
			if mounted {
				if err := n.Mounter.Unmount(p); err != nil {
					t.Errorf("cleanup unmount %s: %v", p, err)
				}
			}
		}
		file, err := os.Open("/proc/self/mountinfo")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		points, err := mountPoints(file)
		if err != nil {
			t.Error(err)
		}
		for _, p := range points {
			if p == n.M.Root || strings.HasPrefix(p, n.M.Root+"/") || p == pods || strings.HasPrefix(p, pods+"/") {
				t.Errorf("test mount remains: %s", p)
			}
		}
	})
	ctx := context.Background()
	ids := []string{"linux-volume-1", "linux-volume-2"}
	var wg sync.WaitGroup
	errs := make([]error, len(targets))
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			_, errs[i] = n.NodePublishVolume(ctx, publishReq(ids[i], target, "a"))
		}(i, target)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("real Linux publish (requires CAP_SYS_ADMIN): %v", err)
		}
	}
	if len(pulls.pulls) != 1 {
		t.Fatalf("%d store pulls, want 1", len(pulls.pulls))
	}
	if users, err := n.M.Users(f.a); err != nil || users != 2 {
		t.Fatalf("shared leases: %d, %v", users, err)
	}
	if _, err := n.NodePublishVolume(ctx, publishReq(ids[0], targets[0], "a")); err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	for _, p := range paths {
		mounted, err := n.Mounter.IsMounted(p)
		if err != nil || !mounted {
			t.Fatalf("%s mounted: %v, %v", p, mounted, err)
		}
		var st unix.Statfs_t
		if err := unix.Statfs(p, &st); err != nil {
			t.Fatal(err)
		}
		want := int64(unix.ST_RDONLY | unix.ST_NOSUID | unix.ST_NODEV)
		if int64(st.Flags)&want != want {
			t.Fatalf("%s flags %#x lack readonly/nosuid/nodev", p, st.Flags)
		}
		if err := os.WriteFile(filepath.Join(p, "must-not-write"), []byte("x"), 0o600); !errors.Is(err, unix.EROFS) {
			t.Fatalf("write %s: %v, want EROFS", p, err)
		}
		sameTree(t, f.srcA, p)
	}
	a, err := os.Stat(filepath.Join(targets[0], "model.bin"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(targets[1], "model.bin"))
	if err != nil || !os.SameFile(a, b) {
		t.Fatalf("shared inode: %v", err)
	}
	t.Log("one real store pull; two leases; identical shared inode; source and targets readonly/nosuid/nodev; writes EROFS")
	for i, target := range targets {
		for retry := 0; retry < 2; retry++ {
			if _, err := n.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{VolumeId: ids[i], TargetPath: target}); err != nil {
				t.Fatalf("unpublish %d retry %d: %v", i, retry, err)
			}
		}
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unpublished target remains: %v", err)
		}
		if users, err := n.M.Users(f.a); err != nil || users != 1-i {
			t.Fatalf("remaining leases: %d, %v", users, err)
		}
		if i == 0 {
			sameTree(t, f.srcA, targets[1])
			if gone, err := n.M.Evict(time.Now().Add(2*time.Hour), time.Hour, n.Mounter.Unmount); err != nil || len(gone) != 0 {
				t.Fatalf("evicted leased artifact: %v, %v", gone, err)
			}
		}
	}
	if records, err := os.ReadDir(filepath.Join(n.M.Root, "volumes")); err != nil || len(records) != 0 {
		t.Fatalf("unpublished volume records remain: %v, %v", records, err)
	}
	if mounted, err := n.Mounter.IsMounted(dir); err != nil || !mounted {
		t.Fatalf("idle source self-bind: %v, %v", mounted, err)
	}
	gone, err := n.M.Evict(time.Now().Add(2*time.Hour), time.Hour, n.Mounter.Unmount)
	if err != nil || len(gone) != 1 || gone[0] != f.a {
		t.Fatalf("eviction: %v, %v", gone, err)
	}
	for _, p := range []string{dir, filepath.Join(n.M.Root, "leases", f.a.Hex())} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("evicted path remains %s: %v", p, err)
		}
	}
	if mounted, err := n.Mounter.IsMounted(dir); err != nil || mounted {
		t.Fatalf("evicted self-bind remains: %v, %v", mounted, err)
	}
	t.Log("publish/unpublish retries succeeded; live lease blocked eviction; final eviction removed self-bind, artifact and lease directory")
}
