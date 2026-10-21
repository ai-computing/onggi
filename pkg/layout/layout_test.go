package layout

import "testing"

func TestOf(t *testing.T) {
	for path, want := range map[string]Kind{
		"store/rbln-llama8b-pd/decode_S512_B4_lm.rbln":                           ATOMDecode,
		"compiled/r1/decoder_batch_8.rbln":                                       ATOMDecode,
		"store/rbln-llama8b-pd/prefill_S512_C512.rbln":                           ATOMPrefill,
		"store/rbln-llama8b-pd/llama8b_S256_fp16.rbln":                           ATOMOther,
		"cache/furiosa/llm/compiled_graphs/0147.edf":                             RNGDGraph,
		"artifacts_cache/2026.2.0/0735.w":                                        RNGDGraph,
		"artifacts/llama8b-fine/artifact.json":                                   RNGDBundle,
		"artifacts/llama8b-fine/binary_bundle.zip":                               RNGDBundle,
		"cache/furiosa/llm/param_files/params-x.safetensors/model-1.safetensors": RNGDParams,
		"artifacts/llama8b-coarse/params-Llama/model-00001-of-00004.safetensors": RNGDParams,
		`hub\models--meta-llama\snapshots\abc\model-00001-of-00004.safetensors`:  HF,
		"hub/models--meta-llama--Llama-3.1-8B-Instruct/blobs/2b1879f356aed35003": HF,
		"artifacts/llama8b-fine/tokenizer.json":                                  Other,
	} {
		if got := Of(path); got != want {
			t.Errorf("Of(%q) = %s, want %s", path, got, want)
		}
	}
}
