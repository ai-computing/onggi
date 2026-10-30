package store

import (
 "bytes"
 "context"
 "encoding/binary"
 "encoding/json"
 "path/filepath"
 "testing"

 "github.com/ai-computing/onggi/pkg/backend"
 "github.com/ai-computing/onggi/pkg/digest"
)

func TestPullPreservesPartialSuffixFilesInReverseManifestOrder(t *testing.T) {
 s:=openStore(t,t.TempDir());src:=t.TempDir();ctx:=context.Background();p:=testProfile()
 write(t,filepath.Join(src,"a"),[]byte("first file"));write(t,filepath.Join(src,"a.onggi-partial"),[]byte("different second file"))
 d,_,err:=s.Push(ctx,src,PushOptions{Profile:&p});if err!=nil {t.Fatal(err)}
 m,err:=s.ReadManifest(ctx,d);if err!=nil {t.Fatal(err)}
 if len(m.Files)!=2 {t.Fatal("wrong fixture")};m.Files[0],m.Files[1]=m.Files[1],m.Files[0];m.Locator=nil
 data,_:=json.Marshal(m);d=digest.Of(data)
 if err=s.B.Put(ctx,backend.Key{Kind:backend.Manifest,Name:d.Hex()},bytes.NewReader(data),int64(len(data)));err!=nil{t.Fatal(err)}
 out:=t.TempDir();if _,err=s.Pull(ctx,d,out,PullOptions{});err!=nil{t.Fatal(err)}
 sameTree(t,src,out)
}

func TestLocatorCountRefusesAllocationAfterInvalidCount(t *testing.T) {
 data:=make([]byte,4);binary.LittleEndian.PutUint32(data,^uint32(0))
 for _,min:=range []int{4,32,41} {
  r:=reader{p:data};if n:=r.count(min);n!=0||r.err==nil {t.Fatalf("unsafe allocation count=%d, min=%d, err=%v",n,min,r.err)}
 }
 // A bounded malformed locator exercises the decoder without a dangerous allocation.
 malformed:=append([]byte("ONGGILC1"),100,0,0,0)
 if _,_,err:=readLocator(malformed,&Manifest{});err==nil {t.Fatal("truncated locator accepted")}
}
