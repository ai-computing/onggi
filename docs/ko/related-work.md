# 관련 연구

2026-10-21 조사 · [English](../related-work.md)

온기는 이미 발표된 기법들을 조합한 시스템입니다. 이 문서는 구성요소별로 가장 가까운 선행 연구와 기술을 정리하고,
온기가 기여로 **주장하지 않는 것**과 차별점을 밝힙니다. 코드 재사용과 라이선스 규칙은
[ADR-0009](../adr/0009-clean-room-and-vendor-formats.md)에 있습니다.

## 1. 내용 기반 분할, 팩, 없는 것만 전송

| 연구·시스템 | 이미 하는 일 |
|---|---|
| LBFS (SOSP 2001) | Rabin 지문 분할로 받는 쪽에 없는 청크만 전송 |
| Data Domain (FAST 2008) | 세그먼트 중복 제거, 인덱스를 가진 컨테이너 |
| FastCDC (USENIX ATC 2016, IEEE TPDS 2020) | gear 해시, 경계 건너뛰기, 정규화 분할(온기 청커가 구현하는 알고리즘) |
| [Hugging Face Xet](https://huggingface.co/docs/xet/index) ([xet-core](https://github.com/huggingface/xet-core), Apache-2.0) | ~64 KiB gear 청크, **64 MiB 이하 xorb**, 파일→청크 구간 shard, 전역 중복 제거, 없는 청크만 업로드, 청크별 LZ4 또는 바이트 그룹화+LZ4 |
| [casync](https://github.com/systemd/casync) / [desync](https://github.com/folbricht/desync) | 청크 인덱스와 저장소, HTTP/S3로 없는 청크만 받기, 로컬 파일을 seed로 사용 |
| [restic](https://github.com/restic/restic), [Borg](https://github.com/borgbackup/borg) | CDC, 인덱스를 가진 팩, 다양한 백엔드 |
| [zsync](http://zsync.moria.org.uk), [zchunk](https://github.com/zchunk/zchunk) | 클라이언트에 없는 부분만 구간 다운로드 |
| ZipLLM (NSDI 2026), TStore (arXiv 2604.17104) | 모델 허브의 텐서 단위 중복 제거. 프레임워크 체크포인트에서는 CDC보다 낫다고 주장 |

**주장하지 않는 것:**
- 청커, 팩, 인덱스, manifest
- CAS로 갱신하는 ref
- 없는 청크만 올리기, 구간 단위 pull

gear 표가 달라서 온기의 청크 경계는 Xet과 호환되지 않습니다. 이는 구현상의 선택일 뿐 기여가 아닙니다.

## 2. bf16/fp16 가중치 무손실 압축 (#8)

| 연구 | 이미 하는 일 |
|---|---|
| HDF5 shuffle 필터, [Blosc](https://github.com/Blosc/c-blosc2) shuffle·bitshuffle | 범용 압축 전에 바이트·비트 평면 분리. 수십 년 된 기법 |
| ZipNN (IEEE CLOUD 2025, [코드](https://github.com/zipnn/zipnn) MIT) | 바이트 그룹화, 지수 Huffman 부호화. bf16 모델 약 33% 축소 |
| DFloat11 (NeurIPS 2025, [코드](https://github.com/LeanModels/DFloat11) Apache-2.0) | bf16 지수 Huffman 부호화, 부호·가수는 그대로, GPU 복원기. 약 30% |
| NeuZip (NeurIPS 2024), Huff-LLM, ECF8 (ICLR 2026) | 지수 엔트로피 부호화. NeuZip 코드는 비상업 라이선스(ADR-0009 참고) |
| Xet `ByteGrouping4LZ4` | 중복 제거 후 CDC 청크마다 바이트 그룹화 |

**주장하지 않는 것:**
- 바이트 평면 분리
- 지수 부호화
- 중복 제거 후 압축

연구실 노드 실측(bf16 Llama-3.1-8B, 텐서당 32 MB 샘플):

| 방식 | 압축률 |
|---|---|
| 원본 바이트에 zlib | 1.26× |
| 바이트 평면 분리 후 zlib | 1.43× |
| 지수 엔트로피 한계 | 1.50–1.51× |

발표된 수치와 일치합니다. 온기가 이 부분에서 하는 일은 새 알고리즘이 아니라 엔지니어링(속도, 팩 구조와의 결합)입니다.

## 3. 유사 파일 간 차분 전송 (#9)

| 연구 | 이미 하는 일 |
|---|---|
| rsync (1996), xdelta3 / VCDIFF (RFC 3284) | 롤링 체크섬 매칭, 기준 파일 대비 차분 부호화 |
| Shilane 외 (FAST 2012), Ddelta, Odess, DeepSketch | 중복 제거 후 유사도 탐지 + 차분 |
| ZipLLM / BitX, TStore | 관련 모델 간 무손실 XOR 차분, 모델 계열 묶기 |
| BitDelta, DeltaZip, ExCP, Delta-DNN | 파인튜닝·체크포인트 간 손실 차분 |

**주장하지 않는 것:**
- 없는 청크만 받기
- 기준 파일 대비 차분
- 유사도 스케치

## 4. Kubernetes 배포

| 연구·시스템 | 이미 하는 일 |
|---|---|
| [Dragonfly](https://github.com/dragonflyoss/dragonfly) / [Nydus](https://github.com/dragonflyoss/nydus), Kraken, Spegel | 이미지·모델의 P2P 배포, 이미지 간 청크 중복 제거 |
| [ModelPack](https://github.com/modelpack/model-spec), KServe Modelcar / LocalModelCache, Kubernetes image volume (KEP-4639) | OCI 아티팩트로서의 모델, 노드 로컬 캐시, 읽기 전용 볼륨 |
| [Fluid](https://github.com/fluid-cloudnative/fluid), JuiceFS | 데이터셋·모델 캐시와 프리페치 |
| ServerlessLLM (OSDI 2024), Run:ai Model Streamer, fastsafetensors | 빠른 로딩, 다계층 캐시, 지역성 기반 배치 |
| [optimum-neuron cache](https://huggingface.co/aws-neuron/optimum-neuron-cache) | 가속기 컴파일 결과를 그래프·옵션 해시로 파일 통째로 캐시 |

**주장하지 않는 것:**
- P2P 배포
- OCI 패키징
- 노드 캐시와 CSI 전달 그 자체

## 5. 온기의 차별점

1. **NPU 컴파일 결과물 자체가 대상**
   - NPU로 컴파일한 LLM의 컴파일 변형 간 중복을 측정한 발표 자료는 찾지 못했습니다.
   - 온기 실측([artifact-formats.md](artifact-formats.md)):
     - ATOM Llama-3.1-8B 빌드 묶음 262 GB → 44.7 GB
     - decode 변형 간 98.5–99.7% 공유, prefill 변형 간 29–37% 공유
     - RNGD 가중치 파일은 HF 원본과 바이트 단위로 동일
     - RNGD 그래프 묶음은 버킷 단위로 겹침
   - 가장 가까운 시스템인 optimum-neuron cache는 파일 통째로 해시해 저장합니다.
2. **컴파일 조합을 계열 축으로 사용**
   - 변형은 파인튜닝이 아니라 컴파일러 버전, 옵션, 버킷, 배치, 병렬 분할에 따라 달라집니다.
   - 배치·프리페치·GC가 이 구조를 활용합니다.
3. **체크포인트와 컴파일 결과물을 한 저장소에서 관리**
   - 공개된 메타데이터와 사용자가 직접 만든 바이트만 사용합니다.
   - 벤더 형식은 해독하지 않습니다(ADR-0009).
4. **이기종 NPU를 위한 Kubernetes + llm-d 통합**
   - 컴파일 팜, 노드 agent와 CSI, 결과물을 고려한 배치

**논문 구성 제안:**
- (1)을 앞세운 "측정 연구 + 시스템"으로 구성합니다.
- 비교 기준선: Xet, ZipLLM/TStore, ZipNN/DFloat11, zstd long-range 모드, image volume / Dragonfly
- (2)–(4)는 시스템 설계로 제시합니다.
