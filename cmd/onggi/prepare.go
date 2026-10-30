package main

import (
 "context"
 "errors"
 "flag"
 "fmt"
 "io"
 "os"
 "os/signal"
 "path/filepath"
 "syscall"

 "github.com/ai-computing/onggi/pkg/backend"
 bfs "github.com/ai-computing/onggi/pkg/backend/fs"
 "github.com/ai-computing/onggi/pkg/digest"
 "github.com/ai-computing/onggi/pkg/store"
)

func readProfileFile(name string) (store.Profile,error) {
 f,err:=os.Open(name)
 if err!=nil {return store.Profile{},err}
 defer f.Close()
 return store.DecodeProfile(f)
}

type prepareReport struct {
 Selection *store.Selection `json:"selection,omitempty"`
 Ready bool `json:"ready"` // verified files on disk, not NPU readiness
 Directory string `json:"directory,omitempty"`
 Stats *store.PullStats `json:"stats,omitempty"`
 Error string `json:"error,omitempty"`
}

func prepare(args []string,stdout,stderr io.Writer) int {
 var report prepareReport
 fail:=func(code int,err error)int {
  report.Error=err.Error();fmt.Fprintln(stderr,"onggi:",err)
  if writeJSON(stdout,stderr,report)!=0 {return 1};return code
 }
 fs:=flag.NewFlagSet("prepare",flag.ContinueOnError);fs.SetOutput(stderr)
 sf:=addStoreFlags(fs)
 request:=fs.String("request","","exact compatibility profile JSON")
 dest:=fs.String("dir","","previously absent destination directory")
 cacheDir:=fs.String("cache","","node chunk cache directory")
 peers:=fs.String("peers","","comma-separated peer URLs")
 workers:=fs.Int("workers",8,"ranges read at once")
 planOnly:=fs.Bool("plan-only",false,"select using metadata; do not fetch model chunks or create destination/cache")
 if err:=fs.Parse(args);err!=nil {return fail(2,err)}
 if fs.NArg()==0||*request==""||(!*planOnly&&*dest=="")||*workers<1 {return fail(2,errors.New("usage: onggi prepare --store S --request FILE [--plan-only | --dir DEST] <ref|digest>...; workers must be positive"))}
 want,err:=readProfileFile(*request)
 if err!=nil {return fail(2,err)}
 ctx,stop:=signal.NotifyContext(context.Background(),os.Interrupt,syscall.SIGTERM);defer stop()
 s,refs,err:=sf.open(ctx)
 if err!=nil {return fail(1,err)}
 release,err:=s.Lease(ctx,"compatible prepare");if err!=nil{return fail(1,err)};defer release()
 candidates:=make([]digest.Digest,0,fs.NArg())
 for _,input:=range fs.Args() {
  d,err:=store.Resolve(ctx,refs,input)
  if err!=nil {return fail(1,fmt.Errorf("candidate %q: %w",input,err))}
  candidates=append(candidates,d)
 }
 selected,err:=s.Select(ctx,candidates,want);report.Selection=&selected
 if err!=nil {return fail(1,err)}
 if *planOnly {return writeJSON(stdout,stderr,report)}
 d,err:=digest.Parse(selected.Manifest);if err!=nil{return fail(1,err)}
 o:=store.PullOptions{Workers:*workers,Peers:peerClients(*peers)}
 if *cacheDir!="" {
  cb,err:=bfs.Open(*cacheDir,backend.DurabilityNone)
  if err==nil {o.Cache,err=store.OpenWith(ctx,cb,store.Create{Chunker:s.Config.Chunker,Sub:s.Config.Sub,SubKinds:s.Config.SubKinds,PackSize:s.Config.PackSize})}
  if err!=nil {return fail(1,fmt.Errorf("node cache: %w",err))}
 }
 st,err:=prepareFiles(ctx,s,d,*dest,o)
 if err!=nil {return fail(1,err)}
 report.Ready=true;report.Directory=*dest;report.Stats=&st
 return writeJSON(stdout,stderr,report)
}

// prepareFiles publishes only a complete tree. It never re-resolves the chosen digest.
func prepareFiles(ctx context.Context,s *store.Store,d digest.Digest,dest string,o store.PullOptions) (store.PullStats,error) {
 var st store.PullStats
 if err:=ctx.Err();err!=nil{return st,err}
 dest=filepath.Clean(dest);parent:=filepath.Dir(dest)
 if err:=os.MkdirAll(parent,0755);err!=nil{return st,err}
 lock:=dest+".onggi-lock"
 if err:=os.Mkdir(lock,0700);err!=nil{return st,fmt.Errorf("destination busy: %w",err)}
 defer os.Remove(lock) // never remove a lock this call did not create
 if _,err:=os.Lstat(dest);!errors.Is(err,os.ErrNotExist) {
  if err==nil {err=errors.New("destination already exists")};return st,err
 }
 tmp,err:=os.MkdirTemp(parent,"."+filepath.Base(dest)+".onggi-")
 if err!=nil{return st,err}
 defer os.RemoveAll(tmp)
 if err=os.Chmod(tmp,0755);err!=nil{return st,err}
 st,err=s.Pull(ctx,d,tmp,o)
 if err!=nil{return st,err}
 if err=ctx.Err();err!=nil{return st,err}
 if _,err=os.Lstat(dest);!errors.Is(err,os.ErrNotExist) {
  if err==nil{err=errors.New("destination appeared during preparation")};return st,err
 }
 if err=os.Rename(tmp,dest);err!=nil{return st,err}
 return st,nil
}
