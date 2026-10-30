package store

import (
 "bytes"
 "context"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "sort"
 "strings"
 "unicode"

 "github.com/ai-computing/onggi/pkg/digest"
)

// Profile is a publisher's exact compatibility declaration, bound to artifact files by the manifest.
// It does not attest vendor ABI compatibility or permit shape padding.
type Profile struct {
 Schema string `json:"schema"`
 ModelSource string `json:"model_source"`
 Options string `json:"options"`
 Device string `json:"device"`
 Runtime string `json:"runtime"`
 SDK string `json:"sdk"`
 Compiler string `json:"compiler"`
 Firmware string `json:"firmware"`
 Format string `json:"format"`
 Layout string `json:"layout"`
 Topology string `json:"topology"`
 TP int `json:"tp"`
 Batch int `json:"batch"`
 Sequence int `json:"sequence"`
}

func (p Profile) Validate() error {
 if p.Schema!="onggi.profile.v1" {return fmt.Errorf("profile: unknown schema %q",p.Schema)}
 for name,value:=range map[string]string{"model_source":p.ModelSource,"options":p.Options} {
  d,err:=digest.Parse(value)
  if err!=nil||d.IsZero()||d.String()!=value {return fmt.Errorf("profile %s: want canonical nonzero sha256 digest",name)}
 }
 for name,value:=range map[string]string{"device":p.Device,"runtime":p.Runtime,"sdk":p.SDK,"compiler":p.Compiler,"firmware":p.Firmware,"format":p.Format,"layout":p.Layout,"topology":p.Topology} {
  if value==""||strings.TrimSpace(value)!=value||value=="*"||strings.EqualFold(value,"unknown")||strings.IndexFunc(value,unicode.IsControl)>=0 {return fmt.Errorf("profile %s: want explicit nonblank identity",name)}
 }
 if p.TP<1||p.Batch<1||p.Sequence<1 {return errors.New("profile: tp, batch and sequence must be positive integers")}
 return nil
}

// UnmarshalJSON also validates profiles embedded in manifests, rejecting future/typo fields.
func (p *Profile) UnmarshalJSON(data []byte) error {
 type plain Profile
 var v plain
 dec:=json.NewDecoder(bytes.NewReader(data));dec.DisallowUnknownFields()
 if err:=dec.Decode(&v);err!=nil {return fmt.Errorf("profile: %w",err)}
 if err:=Profile(v).Validate();err!=nil {return err}
 *p=Profile(v)
 return nil
}

// DecodeProfile reads one bounded JSON object with no trailing content.
func DecodeProfile(r io.Reader) (Profile,error) {
 var p Profile
 data,err:=io.ReadAll(io.LimitReader(r,(64<<10)+1))
 if err!=nil {return p,err}
 if len(data)>64<<10 {return p,errors.New("profile exceeds 64 KiB")}
 err=json.Unmarshal(data,&p)
 return p,err
}

type Rejection struct {
 Manifest string `json:"manifest"`
 Reasons []string `json:"reasons"`
}

type Selection struct {
 Manifest string `json:"manifest,omitempty"`
 Profile Profile `json:"profile"`
 Compatible []string `json:"compatible"`
 Rejected []Rejection `json:"rejected"`
}

var ErrNoCompatible=errors.New("no compatible artifact")

// Select checks every supplied manifest and deterministically chooses the lowest matching digest.
// Callers protect mutable ref resolution and subsequent use with a store GC lease.
func (s *Store) Select(ctx context.Context,candidates []digest.Digest,want Profile) (Selection,error) {
 result:=Selection{Profile:want,Compatible:[]string{},Rejected:[]Rejection{}}
 if err:=want.Validate();err!=nil {return result,err}
 unique:=map[digest.Digest]bool{}
 ds:=append([]digest.Digest(nil),candidates...)
 sort.Slice(ds,func(i,j int)bool{return ds[i].String()<ds[j].String()})
 for _,d:=range ds {
  if err:=ctx.Err();err!=nil {return result,err}
  if unique[d] {continue};unique[d]=true
  m,err:=s.ReadManifest(ctx,d)
  if err!=nil {return result,fmt.Errorf("candidate %s: %w",d,err)}
  if m.Profile==nil {result.Rejected=append(result.Rejected,Rejection{d.String(),[]string{"missing profile"}});continue}
  if *m.Profile==want {result.Compatible=append(result.Compatible,d.String());continue}
  // Raw JSON scalars retain exact integer values in field-level diagnostics.
  a,_:=json.Marshal(want);b,_:=json.Marshal(m.Profile)
  var w,h map[string]json.RawMessage;json.Unmarshal(a,&w);json.Unmarshal(b,&h)
  reasons:=[]string{}
  for key,value:=range w {if !bytes.Equal(value,h[key]) {reasons=append(reasons,key+" differs")}}
  sort.Strings(reasons)
  result.Rejected=append(result.Rejected,Rejection{d.String(),reasons})
 }
 if len(result.Compatible)==0 {return result,ErrNoCompatible}
 result.Manifest=result.Compatible[0]
 return result,nil
}
