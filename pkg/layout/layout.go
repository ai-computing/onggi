// Package layout names what kind of artifact a file is, from its path alone.
//
// The kinds follow docs/artifact-formats.md. They group the report; they do not change how bytes
// are chunked, so a wrong guess only puts a file in the wrong row.
package layout

import (
	"path/filepath"
	"strings"
)

// Kind is an artifact kind. Kinds are reported in the order of All.
type Kind string

const (
	HF          Kind = "hf"           // source checkpoint (safetensors) outside vendor directories
	RNGDParams  Kind = "rngd-params"  // furiosa-llm param_files: the HF bytes re-sharded
	RNGDGraph   Kind = "rngd-graph"   // .edf graphs and .w weight references
	RNGDBundle  Kind = "rngd-bundle"  // artifact.json / binary_bundle.zip
	ATOMDecode  Kind = "atom-decode"  // .rbln decode graphs (weights embedded)
	ATOMPrefill Kind = "atom-prefill" // .rbln prefill graphs (a second weight layout)
	ATOMOther   Kind = "atom-other"   // other .rbln files
	Other       Kind = "other"
)

// All lists the kinds in report order: sources first, so vendor copies show as duplicates of them.
var All = []Kind{HF, RNGDParams, RNGDGraph, RNGDBundle, ATOMDecode, ATOMPrefill, ATOMOther, Other}

// Of classifies a file path.
func Of(path string) Kind {
	p := strings.ToLower(filepath.ToSlash(path))
	base := p[strings.LastIndex(p, "/")+1:]
	ext := filepath.Ext(base)
	switch {
	case ext == ".rbln":
		switch {
		case strings.Contains(base, "prefill"):
			return ATOMPrefill
		case strings.Contains(base, "decode") || strings.Contains(base, "decoder"):
			return ATOMDecode
		}
		return ATOMOther
	case ext == ".edf" || ext == ".w":
		return RNGDGraph
	case base == "artifact.json" || base == "binary_bundle.zip":
		return RNGDBundle
	case ext == ".safetensors":
		if strings.Contains(p, "/param_files/") || strings.Contains(p, "/params-") {
			return RNGDParams
		}
		return HF
	case strings.Contains(p, "/models--") && strings.Contains(p, "/blobs/"):
		return HF // Hugging Face hub cache: snapshot files are links to blobs named by hash
	}
	return Other
}
