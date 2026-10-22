package csi

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

// fakeMounter records bind mounts instead of making them.
type fakeMounter struct {
	mu     sync.Mutex
	mounts map[string]string // target → source
}

func (f *fakeMounter) BindReadOnly(source, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounts[filepath.Clean(target)] = source
	return nil
}

func (f *fakeMounter) Unmount(target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mounts, filepath.Clean(target))
	return nil
}

func (f *fakeMounter) IsMounted(target string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.mounts[filepath.Clean(target)]
	return ok, nil
}

func newNode(t *testing.T) (*Node, *fixture, *counting, *fakeMounter) {
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	fm := &fakeMounter{mounts: map[string]string{}}
	refs := map[string]digest.Digest{"a": f.a, "b": f.b}
	n := &Node{NodeID: "node-1", M: m, Mounter: fm, Logf: t.Logf,
		Resolve: func(ctx context.Context, ref string) (digest.Digest, error) {
			if d, ok := refs[ref]; ok {
				return d, nil
			}
			return store.Resolve(ctx, nil, ref)
		}}
	return n, f, c, fm
}

func publishReq(id, target, ref string) *csipb.NodePublishVolumeRequest {
	return &csipb.NodePublishVolumeRequest{
		VolumeId:   id,
		TargetPath: target,
		Readonly:   true,
		VolumeCapability: &csipb.VolumeCapability{
			AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{}},
			AccessMode: &csipb.VolumeCapability_AccessMode{Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
		VolumeContext: map[string]string{AttrRef: ref, attrEphemeral: "true"},
	}
}

// Two volumes with one ref share one materialized directory; unpublishing unmounts and keeps it.
func TestPublishSharesOneDirectory(t *testing.T) {
	n, f, c, fm := newNode(t)
	ctx := context.Background()
	pods := t.TempDir()
	t1, t2 := filepath.Join(pods, "p1", "mount"), filepath.Join(pods, "p2", "mount")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, tg := range []string{t1, t2} {
		wg.Add(1)
		go func(i int, tg string) {
			defer wg.Done()
			_, errs[i] = n.NodePublishVolume(ctx, publishReq("csi-"+string(rune('1'+i)), tg, "a"))
		}(i, tg)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatal(errs)
	}
	if len(c.pulls) != 1 {
		t.Fatalf("%d pulls, want 1", len(c.pulls))
	}
	dir := n.M.Dir(f.a)
	if fm.mounts[t1] != dir || fm.mounts[t2] != dir {
		t.Fatalf("mounts %v, want both on %s", fm.mounts, dir)
	}
	// publishing again is a no-op
	if _, err := n.NodePublishVolume(ctx, publishReq("csi-1", t1, "a")); err != nil {
		t.Fatal(err)
	}
	if u, _ := n.M.Users(f.a); u != 2 {
		t.Fatalf("%d users, want 2", u)
	}
	for _, v := range []struct{ id, tg string }{{"csi-1", t1}, {"csi-2", t2}, {"csi-2", t2}} { // twice: idempotent
		if _, err := n.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{VolumeId: v.id, TargetPath: v.tg}); err != nil {
			t.Fatal(err)
		}
	}
	if len(fm.mounts) != 0 {
		t.Fatalf("left mounted: %v", fm.mounts)
	}
	if u, _ := n.M.Users(f.a); u != 0 || !isDir(dir) {
		t.Fatalf("%d users, directory kept %v", u, isDir(dir))
	}
}

func TestPublishRejects(t *testing.T) {
	n, _, _, _ := newNode(t)
	target := filepath.Join(t.TempDir(), "mount")
	rw := publishReq("csi-1", target, "a")
	rw.Readonly = false
	noRef := publishReq("csi-1", target, "")
	persistent := publishReq("csi-1", target, "a")
	persistent.VolumeContext[attrEphemeral] = "false"
	block := publishReq("csi-1", target, "a")
	block.VolumeCapability.AccessType = &csipb.VolumeCapability_Block{Block: &csipb.VolumeCapability_BlockVolume{}}
	for name, c := range map[string]struct {
		req  *csipb.NodePublishVolumeRequest
		code codes.Code
	}{
		"read-write":  {rw, codes.InvalidArgument},
		"no ref":      {noRef, codes.InvalidArgument},
		"persistent":  {persistent, codes.InvalidArgument},
		"block":       {block, codes.InvalidArgument},
		"unknown ref": {publishReq("csi-1", target, "nope"), codes.Unavailable},
	} {
		_, err := n.NodePublishVolume(context.Background(), c.req)
		if status.Code(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
}

// The services answer over a unix socket, as kubelet and the registrar reach them.
func TestServeOverUnixSocket(t *testing.T) {
	n, _, _, _ := newNode(t)
	sock := filepath.Join(t.TempDir(), "csi.sock")
	l, err := Listen("unix://" + filepath.ToSlash(sock))
	if err != nil {
		if strings.Contains(err.Error(), "not supported") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	srv := NewServer(n)
	go srv.Serve(l)
	defer srv.Stop()
	conn, err := grpc.NewClient("passthrough:///csi", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	info, err := csipb.NewIdentityClient(conn).GetPluginInfo(context.Background(), &csipb.GetPluginInfoRequest{})
	if err != nil || info.Name != DriverName {
		t.Fatalf("plugin info %v %v", info, err)
	}
	ni, err := csipb.NewNodeClient(conn).NodeGetInfo(context.Background(), &csipb.NodeGetInfoRequest{})
	if err != nil || ni.NodeId != "node-1" {
		t.Fatalf("node info %v %v", ni, err)
	}
}

// A plugin does not take the place of one that answers, and a plugin that exits late leaves its
// successor's socket alone (#61).
func TestListenKeepsTheLiveSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "csi") // short path: unix socket paths are limited to ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "csi.sock")
	old, err := Listen("unix://" + p)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := old.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if l, err := Listen("unix://" + p); err == nil {
		l.Close()
		t.Fatal("took the place of a live plugin")
	}
	// the old plugin hangs; its socket file goes and a new plugin starts
	os.Remove(p)
	cur, err := Listen("unix://" + p)
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	old.Close() // exits late
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the late exit removed the live socket: %v", err)
	}
	cur.Close()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("a plugin left its own socket behind: %v", err)
	}
}
