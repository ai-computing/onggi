package csi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/digest"
)

// DriverName is the CSI driver name pods use in their volumes.
const DriverName = "onggi.ai-computing.github.io"

// Volume attributes.
const (
	AttrRef       = "ref"                          // ref name or "sha256:..." manifest digest
	attrEphemeral = "csi.storage.k8s.io/ephemeral" // set by kubelet for inline volumes
)

// Node serves the CSI Identity and Node services. Volumes are ephemeral inline volumes only:
// NodePublishVolume materializes the artifact (once per node) and bind-mounts it read-only.
type Node struct {
	csipb.UnimplementedIdentityServer
	csipb.UnimplementedNodeServer

	Version string
	NodeID  string
	M       *Materializer
	// Resolve turns a volume's ref into a manifest digest (refs live in the driver's store; pods
	// never name a store or carry credentials).
	Resolve func(ctx context.Context, ref string) (digest.Digest, error)
	Mounter Mounter
	Logf    func(format string, args ...any)

	locks sync.Map // volume id → *sync.Mutex: one operation per volume at a time
}

func (n *Node) logf(format string, args ...any) {
	if n.Logf != nil {
		n.Logf(format, args...)
	}
}

func (n *Node) lock(id string) func() {
	v, _ := n.locks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// GetPluginInfo implements the Identity service.
func (n *Node) GetPluginInfo(context.Context, *csipb.GetPluginInfoRequest) (*csipb.GetPluginInfoResponse, error) {
	return &csipb.GetPluginInfoResponse{Name: DriverName, VendorVersion: n.Version}, nil
}

// GetPluginCapabilities implements the Identity service: there is no controller service.
func (n *Node) GetPluginCapabilities(context.Context, *csipb.GetPluginCapabilitiesRequest) (*csipb.GetPluginCapabilitiesResponse, error) {
	return &csipb.GetPluginCapabilitiesResponse{}, nil
}

// Probe implements the Identity service.
func (n *Node) Probe(context.Context, *csipb.ProbeRequest) (*csipb.ProbeResponse, error) {
	return &csipb.ProbeResponse{}, nil
}

// NodeGetInfo implements the Node service.
func (n *Node) NodeGetInfo(context.Context, *csipb.NodeGetInfoRequest) (*csipb.NodeGetInfoResponse, error) {
	return &csipb.NodeGetInfoResponse{NodeId: n.NodeID}, nil
}

// NodeGetCapabilities implements the Node service: no staging, no stats, no expansion.
func (n *Node) NodeGetCapabilities(context.Context, *csipb.NodeGetCapabilitiesRequest) (*csipb.NodeGetCapabilitiesResponse, error) {
	return &csipb.NodeGetCapabilitiesResponse{}, nil
}

// NodePublishVolume materializes the volume's artifact if the node lacks it and bind-mounts its
// directory read-only at the target path.
func (n *Node) NodePublishVolume(ctx context.Context, req *csipb.NodePublishVolumeRequest) (*csipb.NodePublishVolumeResponse, error) {
	id, target := req.GetVolumeId(), req.GetTargetPath()
	if id == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and target path are required")
	}
	if c := req.GetVolumeCapability(); c == nil || c.GetMount() == nil {
		return nil, status.Error(codes.InvalidArgument, "onggi volumes are mount volumes (no block access)")
	}
	vc := req.GetVolumeContext()
	if vc[attrEphemeral] == "false" {
		return nil, status.Error(codes.InvalidArgument, "onggi serves ephemeral inline volumes only")
	}
	if !req.GetReadonly() {
		return nil, status.Error(codes.InvalidArgument, "onggi volumes are read-only: set readOnly: true")
	}
	ref := vc[AttrRef]
	if ref == "" {
		return nil, status.Errorf(codes.InvalidArgument, "volume attribute %q is required", AttrRef)
	}
	defer n.lock(id)()

	if ok, err := n.Mounter.IsMounted(target); err != nil {
		return nil, status.Errorf(codes.Internal, "check %s: %v", target, err)
	} else if ok {
		return &csipb.NodePublishVolumeResponse{}, nil // published before
	}
	d, err := n.Resolve(ctx, ref)
	if err != nil {
		code := codes.Unavailable
		if errors.Is(err, backend.ErrNotFound) {
			code = codes.NotFound
		}
		return nil, status.Errorf(code, "ref %q: %v", ref, err)
	}
	dir, err := n.M.Ensure(ctx, d)
	if err != nil {
		if ctx.Err() != nil {
			// kubelet gave up waiting; the pull goes on and its retry will find or join it
			return nil, status.Errorf(codes.Aborted, "materializing %s: still running", d)
		}
		return nil, status.Errorf(codes.Internal, "materializing %s: %v", d, err)
	}
	if err := n.M.Acquire(id, d, target); err != nil {
		return nil, status.Errorf(codes.Internal, "lease: %v", err)
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		n.M.Release(id)
		return nil, status.Errorf(codes.Internal, "target: %v", err)
	}
	if err := n.Mounter.BindReadOnly(dir, target); err != nil {
		n.M.Release(id)
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	users, _ := n.M.Users(d)
	n.logf("published %s ref %s manifest %s at %s (%d volumes use it)", id, ref, d, target, users)
	return &csipb.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts the target path and drops the volume's lease. The artifact's
// directory stays on the node (eviction is P3).
func (n *Node) NodeUnpublishVolume(ctx context.Context, req *csipb.NodeUnpublishVolumeRequest) (*csipb.NodeUnpublishVolumeResponse, error) {
	id, target := req.GetVolumeId(), req.GetTargetPath()
	if id == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and target path are required")
	}
	defer n.lock(id)()
	if ok, err := n.Mounter.IsMounted(target); err != nil {
		return nil, status.Errorf(codes.Internal, "check %s: %v", target, err)
	} else if ok {
		if err := n.Mounter.Unmount(target); err != nil {
			return nil, status.Errorf(codes.Internal, "%v", err)
		}
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.Internal, "remove %s: %v", target, err)
	}
	d, users, ok, err := n.M.Release(id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "lease: %v", err)
	}
	if ok {
		n.logf("unpublished %s manifest %s from %s (%d volumes use it)", id, d, target, users)
	} else {
		n.logf("unpublished %s from %s (no lease)", id, target)
	}
	return &csipb.NodeUnpublishVolumeResponse{}, nil
}

// Listen opens a CSI endpoint: unix:///path/to/csi.sock or a plain path. A stale socket file is
// removed first.
func Listen(endpoint string) (net.Listener, error) {
	p := endpoint
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
		if u.Scheme != "unix" {
			return nil, fmt.Errorf("endpoint %q: want unix://", endpoint)
		}
		p = u.Path
		if u.Host != "" {
			p = filepath.Join(u.Host, p)
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return nil, err
	}
	// a socket that answers belongs to a live plugin: do not take its place (#61)
	if c, err := net.DialTimeout("unix", p, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("endpoint %s: another plugin is listening there", p)
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	ul := l.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	fi, err := os.Stat(p)
	if err != nil {
		l.Close()
		return nil, err
	}
	return &socket{UnixListener: ul, path: p, fi: fi}, nil
}

// socket removes its path on Close only while the path is still this socket: a plugin that
// exits late must not remove the socket of the plugin that replaced it (#61).
type socket struct {
	*net.UnixListener
	path string
	fi   os.FileInfo
	once sync.Once
}

func (s *socket) Close() error {
	err := s.UnixListener.Close()
	s.once.Do(func() {
		if fi, e := os.Stat(s.path); e == nil && os.SameFile(fi, s.fi) {
			os.Remove(s.path)
		}
	})
	return err
}

// NewServer returns a gRPC server with n's Identity and Node services. Every failed call is
// logged.
func NewServer(n *Node) *grpc.Server {
	s := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		resp, err := h(ctx, req)
		if err != nil {
			n.logf("%s: %v", info.FullMethod, err)
		}
		return resp, err
	}))
	csipb.RegisterIdentityServer(s, n)
	csipb.RegisterNodeServer(s, n)
	return s
}
