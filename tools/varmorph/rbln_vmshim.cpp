// C shim over the public NPU-B runtime API (rebel/runtime/api/rbln_runtime_api.h, SDK 0.12.0) for
// batched copies, which torch-rbln does not bind to Python. Loaded with ctypes inside a process
// that has imported torch_rbln; tensor data pointers are vmem virtual addresses.
//   g++ -O2 -shared -fPIC -I$SP/rebel/include rbln_vmshim.cpp -o librbln_vmshim.so \
//       -L$SP/tvm -l:librbln.so -Wl,-rpath,$SP/tvm
#include <rebel/runtime/api/rbln_runtime_api.h>

#include <cstdint>
#include <tuple>
#include <vector>

extern "C" {

// n copies (src_vaddr[i] -> dst_vaddr[i], size[i]); on the device for <= 1024 per destination.
int vmshim_v2v_multi(const uint64_t* src, const uint64_t* dst, const uint64_t* size, int n) {
  std::vector<std::tuple<uint64_t, uint64_t, uint64_t>> v;
  v.reserve(n);
  for (int i = 0; i < n; ++i) v.emplace_back(src[i], dst[i], size[i]);
  return static_cast<int>(rbln::rbln_memcpy_v2v_multi(v));
}

// n patches (host pointer -> dst_vaddr, size).
int vmshim_h2v_multi(const uint64_t* host, const uint64_t* dst, const uint64_t* size, int n) {
  std::vector<std::tuple<uintptr_t, uint64_t, uint64_t>> v;
  v.reserve(n);
  for (int i = 0; i < n; ++i) v.emplace_back(static_cast<uintptr_t>(host[i]), dst[i], size[i]);
  return static_cast<int>(rbln::rbln_memcpy_h2v_multi(v));
}

int vmshim_layout_like(uint64_t target, uint64_t ref) {
  return static_cast<int>(rbln::rbln_set_device_alloc_layout_like(target, ref));
}

int vmshim_sync(uint32_t torch_device_id) {
  return static_cast<int>(rbln::rbln_device_synchronize(torch_device_id));
}

}  // extern "C"
