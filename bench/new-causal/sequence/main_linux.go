//go:build linux

// Benchmark-only stateful caller; production materializer/store stay unchanged.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/csi"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

type expectedFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type artifact struct {
	Manifest string         `json:"manifest"`
	Files    []expectedFile `json:"files"`
}

type configuration struct {
	Status               string              `json:"status"`
	Kind                 string              `json:"kind"`
	Policy               string              `json:"policy"`
	Store                string              `json:"store"`
	Root                 string              `json:"root"`
	Artifacts            map[string]artifact `json:"artifacts"`
	Requests             []string            `json:"requests"`
	Workers              int                 `json:"workers"`
	IdleNS               int64               `json:"idle_ns"`
	StepTimeoutNS        int64               `json:"step_timeout_ns"`
	SequenceTimeoutNS    int64               `json:"sequence_timeout_ns"`
	MaterializedCapacity int64               `json:"materialized_capacity_bytes"`
	WorkspaceCapacity    int64               `json:"workspace_capacity_bytes"`
	CacheCapacity        int64               `json:"cache_capacity_bytes"`
	BinarySHA256         string              `json:"binary_sha256,omitempty"`
}

type pullRecord struct {
	Offered    int             `json:"offered_before_policy"`
	ClaimCalls int             `json:"claim_calls"`
	Admitted   bool            `json:"admitted"`
	Stats      store.PullStats `json:"stats"`
	Error      string          `json:"error,omitempty"`
}

type fileIdentity struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Bytes  int64  `json:"bytes"`
}

type occupancy struct {
	AtNS         int64    `json:"at_ns"`
	Materialized int64    `json:"materialized_bytes"`
	Workspace    int64    `json:"workspace_bytes"`
	Cache        int64    `json:"cache_bytes"`
	Manifests    []string `json:"manifests"`
}

type requestRecord struct {
	Artifact       string         `json:"artifact"`
	Status         string         `json:"status"`
	AlreadyPresent bool           `json:"already_present"`
	RequestNS      int64          `json:"request_ns"`
	RequestUTC     string         `json:"request_utc"`
	ReleaseNS      int64          `json:"release_ns"`
	IdleFinishNS   int64          `json:"idle_finish_ns"`
	EnsureStartNS  int64          `json:"ensure_start_ns"`
	EnsureFinishNS int64          `json:"ensure_finish_ns"`
	SyncFinishNS   int64          `json:"sync_finish_ns"`
	HashFinishNS   int64          `json:"hash_finish_ns"`
	LeaseNS        int64          `json:"lease_ns"`
	TargetSHAEqual bool           `json:"target_sha_equal"`
	FileIdentities []fileIdentity `json:"file_identities"`
	Pulls          []pullRecord   `json:"pulls"`
	Before         *occupancy     `json:"before,omitempty"`
	After          *occupancy     `json:"after,omitempty"`
	Error          string         `json:"error,omitempty"`
}

type execution struct {
	Schema                 string          `json:"schema"`
	Status                 string          `json:"status"`
	StartedUTC             string          `json:"started_utc"`
	ConfigSHA256           string          `json:"config_sha256"`
	Config                 configuration   `json:"config"`
	OriginConfig           store.Config    `json:"origin_config"`
	Steps                  []requestRecord `json:"steps"`
	SequenceStartNS        int64           `json:"sequence_start_ns"`
	SequenceFinishNS       int64           `json:"sequence_finish_ns"`
	SequenceElapsedNS      int64           `json:"sequence_elapsed_ns"`
	PreparationInclusiveNS int64           `json:"preparation_inclusive_ns"`
	ScientificAcceptance   string          `json:"scientific_acceptance"`
	Scope                  string          `json:"scope"`
	Error                  string          `json:"error,omitempty"`
}

func validHash(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32 && strings.ToLower(value) == value
}

func contained(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Admission is for the retention-allowed experiment, not a hard filesystem quota.
func validate(cfg *configuration) error {
	if cfg.Status != "not_run" || (cfg.Kind != "fixture" && cfg.Kind != "model") ||
		(cfg.Policy != "P" && cfg.Policy != "C" && cfg.Policy != "R") || cfg.Workers < 1 ||
		cfg.IdleNS <= 0 || cfg.StepTimeoutNS <= cfg.IdleNS || cfg.StepTimeoutNS > int64(10*time.Minute) ||
		cfg.SequenceTimeoutNS <= 0 || cfg.SequenceTimeoutNS > int64(30*time.Minute) || cfg.CacheCapacity <= 0 {
		return errors.New("invalid frozen policy, status, kind, resources or deadlines")
	}
	if len(cfg.Artifacts) != 2 || len(cfg.Requests) != 3 || cfg.Requests[0] != cfg.Requests[2] || cfg.Requests[0] == cfg.Requests[1] {
		return errors.New("exactly two artifacts and an A/B/A cycle are required")
	}
	var total int64
	for _, key := range cfg.Requests[:2] {
		a, ok := cfg.Artifacts[key]
		if !ok || len(a.Files) == 0 {
			return errors.New("request lacks complete expected artifact files")
		}
		if _, err := digest.Parse(a.Manifest); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, f := range a.Files {
			if !fs.ValidPath(f.Path) || f.Path == "." || strings.Contains(f.Path, "\\") ||
				seen[f.Path] || f.Bytes < 0 || !validHash(f.SHA256) || f.Bytes > math.MaxInt64-total {
				return errors.New("unsafe, duplicate or invalid expected file")
			}
			seen[f.Path] = true
			total += f.Bytes
		}
	}
	if cfg.Artifacts[cfg.Requests[0]].Manifest == cfg.Artifacts[cfg.Requests[1]].Manifest || total == 0 || cfg.MaterializedCapacity < total || cfg.WorkspaceCapacity <= 0 {
		return errors.New("distinct artifacts and room to retain both are required")
	}
	storeAbs, err := filepath.Abs(cfg.Store)
	if err != nil {
		return err
	}
	storeAbs, err = filepath.EvalSymlinks(storeAbs)
	if err != nil {
		return err
	}
	info, err := os.Stat(storeAbs)
	if err != nil || !info.IsDir() {
		return errors.New("origin must be an existing directory")
	}
	rootAbs, err := filepath.Abs(cfg.Root)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(rootAbs))
	if err != nil {
		return err
	}
	rootAbs = filepath.Join(parent, filepath.Base(rootAbs))
	if _, err := os.Lstat(rootAbs); !os.IsNotExist(err) {
		return errors.New("output root must be new; never restart or overwrite")
	}
	if contained(storeAbs, rootAbs) || contained(rootAbs, storeAbs) {
		return errors.New("origin and owned output must be disjoint")
	}
	cfg.Store, cfg.Root = storeAbs, rootAbs
	return nil
}

func fileHash(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	buffer := make([]byte, 4<<20)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", size, err
		}
		n, err := f.Read(buffer)
		if n > 0 {
			h.Write(buffer[:n])
			size += int64(n)
		}
		if err == io.EOF {
			return hex.EncodeToString(h.Sum(nil)), size, ctx.Err()
		}
		if err != nil {
			return "", size, err
		}
	}
}

func syncAndVerify(ctx context.Context, dir string, files []expectedFile, start time.Time, step *requestRecord) error {
	expected := map[string]expectedFile{}
	for _, f := range files {
		expected[f.Path] = f
	}
	actual := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("non-regular target file")
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := expected[rel]; !ok {
			return fmt.Errorf("unexpected target file %s", rel)
		}
		actual[rel] = true
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("target file inventory incomplete")
	}
	step.SyncFinishNS = time.Since(start).Nanoseconds()
	for _, expected := range files {
		path := filepath.Join(dir, filepath.FromSlash(expected.Path))
		before, err := os.Lstat(path)
		if err != nil {
			return err
		}
		digest, size, err := fileHash(ctx, path)
		if err != nil {
			return err
		}
		after, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !before.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || size != expected.Bytes || digest != expected.SHA256 {
			return fmt.Errorf("target hash/identity mismatch: %s", expected.Path)
		}
		st := after.Sys().(*syscall.Stat_t)
		step.FileIdentities = append(step.FileIdentities, fileIdentity{Path: expected.Path, Device: uint64(st.Dev), Inode: st.Ino, Bytes: size})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	step.TargetSHAEqual = true
	step.HashFinishNS = time.Since(start).Nanoseconds()
	return nil
}

func treeBytes(root string) (int64, error) {
	seen := map[[2]uint64]bool{}
	var size int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("non-regular occupancy entry")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		key := [2]uint64{uint64(st.Dev), st.Ino}
		if !seen[key] {
			seen[key] = true
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func snapshot(m *csi.Materializer, cache string, start time.Time) (*occupancy, error) {
	ds, err := m.Materialized()
	if err != nil {
		return nil, err
	}
	row := &occupancy{AtNS: time.Since(start).Nanoseconds(), Manifests: []string{}}
	for _, d := range ds {
		n, err := treeBytes(m.Dir(d))
		if err != nil {
			return nil, err
		}
		row.Materialized += n
		row.Manifests = append(row.Manifests, d.String())
	}
	sort.Strings(row.Manifests)
	if row.Workspace, err = treeBytes(filepath.Join(m.Root, "tmp")); err != nil {
		return nil, err
	}
	if row.Cache, err = treeBytes(cache); err != nil {
		return nil, err
	}
	return row, nil
}

func saveRecord(root string, report *execution) error {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(root, "record-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	_, err = f.Write(append(raw, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, filepath.Join(root, "result.json"))
}

func execute(cfg configuration, raw []byte, origin *store.Store, start time.Time) (*execution, error) {
	if err := os.Mkdir(cfg.Root, 0o700); err != nil {
		return nil, err
	}
	report := &execution{Schema: "onggi.file-sequence.v1", Status: "running", StartedUTC: start.UTC().Format(time.RFC3339Nano),
		ConfigSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), Config: cfg, OriginConfig: origin.Config,
		Steps: []requestRecord{}, ScientificAcceptance: "external cohort admission required",
		Scope: "Actual CPU filesystem execution; phase snapshots, not peak or physical I/O. No native/service readiness, constrained-capacity policy or automatic recovery."}
	finish := func(err error) (*execution, error) {
		if err != nil {
			report.Status = "failed"
			report.Error = err.Error()
		} else {
			report.Status = "succeeded"
		}
		if saveErr := saveRecord(cfg.Root, report); saveErr != nil {
			return report, saveErr
		}
		return report, err
	}
	cachePath := filepath.Join(cfg.Root, "cache")
	cb, err := bfs.Open(cachePath, backend.DurabilityNone)
	if err != nil {
		return finish(err)
	}
	cache, err := store.OpenWith(context.Background(), cb, store.Create{Chunker: origin.Config.Chunker, Sub: origin.Config.Sub, SubKinds: origin.Config.SubKinds, PackSize: origin.Config.PackSize})
	if err != nil {
		return finish(err)
	}
	m := &csi.Materializer{Root: filepath.Join(cfg.Root, "owner"), RebaseIdle: time.Duration(cfg.IdleNS), MaxPulls: 1}
	var active *requestRecord
	var pullContext context.Context
	m.Pull = func(_ context.Context, md digest.Digest, dest string, offer *csi.Offer) (store.PullStats, error) {
		p := pullRecord{}
		o := store.PullOptions{Workers: cfg.Workers, Cache: cache, CacheTmp: filepath.Join(m.Root, "tmp")}
		if offer != nil {
			p.Offered = len(offer.Bases)
			if cfg.Policy != "P" {
				o.Bases = offer.Bases
				o.Claim = func(b store.Base) (string, bool) {
					p.ClaimCalls++
					if cfg.Policy == "C" {
						return "", false
					}
					dir, ok := offer.Claim(b)
					p.Admitted = ok
					return dir, ok
				}
			}
		}
		st, err := origin.Pull(pullContext, md, dest, o)
		p.Stats = st
		if err != nil {
			p.Error = err.Error()
		}
		active.Pulls = append(active.Pulls, p)
		return st, err
	}
	if err := m.Init(); err != nil {
		return finish(err)
	}
	var previousLease string
	var sequenceContext context.Context
	for i, key := range cfg.Requests {
		requestTime := time.Now()
		step := requestRecord{Artifact: key, Status: "running", RequestNS: requestTime.Sub(start).Nanoseconds(), RequestUTC: requestTime.UTC().Format(time.RFC3339Nano), Pulls: []pullRecord{}, FileIdentities: []fileIdentity{}}
		active = &step
		if i == 1 {
			report.SequenceStartNS = step.RequestNS
			var cancelSequence context.CancelFunc
			sequenceContext, cancelSequence = context.WithDeadline(context.Background(), requestTime.Add(time.Duration(cfg.SequenceTimeoutNS)))
			defer cancelSequence()
		}
		parent := context.Background()
		if i > 0 {
			parent = sequenceContext
		}
		ctx, cancel := context.WithDeadline(parent, requestTime.Add(time.Duration(cfg.StepTimeoutNS)))
		pullContext = ctx
		a := cfg.Artifacts[key]
		md, _ := digest.Parse(a.Manifest)
		var stepErr error
		func() {
			if previousLease != "" {
				_, users, ok, err := m.Release(previousLease)
				if err != nil {
					stepErr = err
					return
				}
				if !ok || users != 0 {
					stepErr = errors.New("previous real lease was not exclusively released")
					return
				}
			}
			step.ReleaseNS = time.Since(start).Nanoseconds()
			if info, err := os.Stat(m.Dir(md)); err == nil {
				step.AlreadyPresent = info.IsDir()
			} else if !os.IsNotExist(err) {
				stepErr = err
				return
			}
			// A retained target and P reconstruction do not need to await donor age.
			if !step.AlreadyPresent && cfg.Policy != "P" && i > 0 {
				timer := time.NewTimer(time.Duration(cfg.IdleNS))
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					stepErr = ctx.Err()
					return
				}
			}
			step.IdleFinishNS = time.Since(start).Nanoseconds()
			step.Before, stepErr = snapshot(m, cachePath, start)
			if stepErr != nil {
				return
			}
			step.EnsureStartNS = time.Since(start).Nanoseconds()
			// Wait for the real owner to finish, rather than return while its
			// background construction is still mutating the recorded state.
			dir, err := m.Ensure(context.Background(), md)
			step.EnsureFinishNS = time.Since(start).Nanoseconds()
			if err != nil {
				stepErr = err
				return
			}
			if err := syncAndVerify(ctx, dir, a.Files, start, &step); err != nil {
				stepErr = err
				return
			}
			lease := fmt.Sprintf("sequence-request-%d", i)
			if err := m.Acquire(lease, md, dir); err != nil {
				stepErr = err
				return
			}
			previousLease = lease
			step.LeaseNS = time.Since(start).Nanoseconds()
			step.After, stepErr = snapshot(m, cachePath, start)
			if stepErr != nil {
				return
			}
			if step.After.Materialized > cfg.MaterializedCapacity || step.After.Cache > cfg.CacheCapacity || step.After.Workspace > cfg.WorkspaceCapacity {
				stepErr = errors.New("phase occupancy exceeds declared allowance; not a constrained-capacity success")
				return
			}
			step.Status = "succeeded"
		}()
		cancel()
		if stepErr != nil {
			step.Status = "failed"
			step.Error = stepErr.Error()
		}
		report.Steps = append(report.Steps, step)
		if stepErr != nil {
			return finish(stepErr)
		}
		if i == 2 {
			report.SequenceFinishNS = step.HashFinishNS
			report.SequenceElapsedNS = report.SequenceFinishNS - report.SequenceStartNS
			report.PreparationInclusiveNS = report.SequenceFinishNS
		}
		if err := saveRecord(cfg.Root, report); err != nil {
			return finish(err)
		}
	}
	return finish(nil)
}

func runSequence(args []string, stdout, stderr io.Writer) int {
	start := time.Now()
	f := flag.NewFlagSet("file-sequence", flag.ContinueOnError)
	f.SetOutput(stderr)
	configPath := f.String("config", "", "explicit frozen sequence config")
	configHash := f.String("config-sha", "", "external SHA pin; required for model execution")
	if err := f.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" || f.NArg() != 0 {
		fmt.Fprintln(stderr, "--config is required")
		return 2
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var cfg configuration
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := validate(&cfg); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if cfg.Kind == "model" {
		if !validHash(*configHash) || fmt.Sprintf("%x", sha256.Sum256(raw)) != *configHash || !validHash(cfg.BinarySHA256) {
			fmt.Fprintln(stderr, "model requires external config and executable SHA pins")
			return 2
		}
		binary, err := os.Executable()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		digest, _, err := fileHash(context.Background(), binary)
		if err != nil || digest != cfg.BinarySHA256 {
			fmt.Fprintln(stderr, "model executable identity differs")
			return 2
		}
	}
	b, err := bfs.Open(cfg.Store, backend.DurabilityNone)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	origin, err := store.OpenWith(context.Background(), b, store.Create{})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var largest int64
	for _, a := range cfg.Artifacts {
		md, _ := digest.Parse(a.Manifest)
		manifest, err := origin.ReadManifest(context.Background(), md)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		want := map[string]int64{}
		var size int64
		for _, f := range a.Files {
			want[f.Path] = f.Bytes
			size += f.Bytes
		}
		if len(manifest.Files) != len(want) {
			fmt.Fprintln(stderr, "expected files differ from manifest")
			return 2
		}
		for _, f := range manifest.Files {
			if n, ok := want[f.Path]; !ok || n != f.Size {
				fmt.Fprintln(stderr, "manifest path/size differs")
				return 2
			}
		}
		if size > largest {
			largest = size
		}
	}
	if origin.Config.PackSize > math.MaxInt64-largest || cfg.WorkspaceCapacity < largest+origin.Config.PackSize {
		fmt.Fprintln(stderr, "workspace must reserve a target plus one declared cache pack")
		return 2
	}
	report, err := execute(cfg, raw, origin, start)
	if report != nil {
		if encodeErr := json.NewEncoder(stdout).Encode(report); encodeErr != nil {
			fmt.Fprintln(stderr, encodeErr)
			return 1
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func main() { os.Exit(runSequence(os.Args[1:], os.Stdout, os.Stderr)) }
