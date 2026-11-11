package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/digest"
)

func testProfile() Profile {
	return Profile{Schema: "onggi.profile.v1", ModelSource: digest.Of([]byte("source")).String(), Options: digest.Of([]byte("options")).String(), Device: "rngd", Runtime: "rngd-pd", SDK: "2026.4.0", Compiler: "2026.4.0", Firmware: "fw1", Format: "runnable", Layout: "rngd-bf16", Topology: "fusion8-mesh2", TP: 1, Batch: 4, Sequence: 256}
}

func profileManifest(t *testing.T, s *Store, p *Profile) digest.Digest {
	t.Helper()
	data, err := json.Marshal(Manifest{Schema: "onggi.manifest.v1", Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	d := digest.Of(data)
	if err = s.B.Put(context.Background(), backend.Key{Kind: backend.Manifest, Name: d.Hex()}, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSelectRequiresEveryIdentityField(t *testing.T) {
	ctx := context.Background()
	want := testProfile()
	for i := 1; i < reflect.TypeOf(want).NumField(); i++ {
		field := reflect.TypeOf(want).Field(i)
		t.Run(field.Name, func(t *testing.T) {
			s := openStore(t, t.TempDir())
			other := want
			v := reflect.ValueOf(&other).Elem().Field(i)
			if v.Kind() == reflect.String {
				if field.Name == "ModelSource" || field.Name == "Options" {
					v.SetString(digest.Of([]byte("other")).String())
				} else {
					v.SetString("different")
				}
			} else {
				v.SetInt(v.Int() + 1)
			}
			rejected := profileManifest(t, s, &other)
			got, err := s.Select(ctx, []digest.Digest{rejected}, want)
			if !errors.Is(err, ErrNoCompatible) || got.Manifest != "" || len(got.Rejected) != 1 {
				t.Fatalf("unsafe match: %+v %v", got, err)
			}
			if len(got.Rejected[0].Reasons) != 1 {
				t.Fatalf("missing field reason: %+v", got.Rejected)
			}
		})
	}
}

func TestSelectLegacyAndDeterministicChoice(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx := context.Background()
	want := testProfile()
	legacy := profileManifest(t, s, nil)
	a := profileManifest(t, s, &want)
	m, _ := s.ReadManifest(ctx, a)
	m.Labels = map[string]string{"build": "second"}
	bdata, _ := json.Marshal(m)
	b := digest.Of(bdata)
	if err := s.B.Put(ctx, backend.Key{Kind: backend.Manifest, Name: b.Hex()}, bytes.NewReader(bdata), int64(len(bdata))); err != nil {
		t.Fatal(err)
	}
	expected := []string{a.String(), b.String()}
	sort.Strings(expected)
	for _, candidates := range [][]digest.Digest{{a, legacy, b, a}, {b, a, legacy}} {
		got, err := s.Select(ctx, candidates, want)
		if err != nil || got.Manifest != expected[0] || !reflect.DeepEqual(got.Compatible, expected) || len(got.Rejected) != 1 {
			t.Fatalf("selection: %+v %v", got, err)
		}
	}
	out := t.TempDir()
	if _, err := s.Pull(ctx, legacy, out, PullOptions{}); err != nil {
		t.Fatalf("legacy pull broke: %v", err)
	}
}

func TestSelectFailsClosedOnUnreadableManifest(t *testing.T) {
	s := openStore(t, t.TempDir())
	want := testProfile()
	good := profileManifest(t, s, &want)
	bad := digest.Of([]byte("missing"))
	if got, err := s.Select(context.Background(), []digest.Digest{good, bad}, want); err == nil || got.Manifest != "" {
		t.Fatalf("unreadable candidate silently ignored: %+v %v", got, err)
	}
}

func TestProfileRejectsIncompleteAndUnknownJSON(t *testing.T) {
	p := testProfile()
	data, _ := json.Marshal(p)
	var base map[string]any
	json.Unmarshal(data, &base)
	for key := range base {
		t.Run("missing-"+key, func(t *testing.T) {
			m := map[string]any{}
			for k, v := range base {
				if k != key {
					m[k] = v
				}
			}
			b, _ := json.Marshal(m)
			if _, err := DecodeProfile(bytes.NewReader(b)); err == nil {
				t.Fatal("incomplete identity accepted")
			}
		})
	}
	invalid := []string{`null`, `{}`, string(data) + ` {}`, strings.Replace(string(data), `"sdk":`, `"typo":`, 1), strings.Replace(string(data), `"batch":4`, `"batch":0`, 1), strings.Replace(string(data), `"batch":4`, `"batch":1.5`, 1), strings.Replace(string(data), `"device":"rngd"`, `"device":" rngd"`, 1), strings.Replace(string(data), `"onggi.profile.v1"`, `"onggi.profile.v2"`, 1), strings.Replace(string(data), p.Options, strings.Repeat("0", 64), 1), strings.Replace(string(data), p.ModelSource, strings.TrimPrefix(p.ModelSource, "sha256:"), 1)}
	for _, b := range invalid {
		if _, err := DecodeProfile(strings.NewReader(b)); err == nil {
			t.Fatalf("invalid profile accepted: %s", b)
		}
	}
	if _, err := DecodeProfile(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func TestPushProfileRoundTripAndRejectsInvalidBeforeStoreWrites(t *testing.T) {
	s := openStore(t, t.TempDir())
	root := t.TempDir()
	write(t, root+"/model.bin", []byte("compiled"))
	p := testProfile()
	d, _, err := s.Push(context.Background(), root, PushOptions{Profile: &p})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.ReadManifest(context.Background(), d)
	if err != nil || m.Profile == nil || *m.Profile != p {
		t.Fatalf("profile lost: %+v %v", m, err)
	}
	p.SDK = ""
	if _, _, err := s.Push(context.Background(), root, PushOptions{Profile: &p}); err == nil {
		t.Fatal("invalid profile published")
	}
}
