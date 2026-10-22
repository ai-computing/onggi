# Onggi

**Kubernetes + llm-d 환경을 위한 모델 가중치·가속기 컴파일 결과물 내용 주소 저장소**

[English](README.md) · 한국어

> 상태: **P1.** 설계는 [docs/ko/design.md](docs/ko/design.md)에 있습니다. `onggi dedup-report`로 저장소가 얼마나
> 줄어드는지 측정하고, `onggi push` / `pull`로 POSIX 마운트나 S3 서버에 결과물을 저장·복원합니다. 클러스터
> 구성요소(노드 agent, CSI 드라이버, 컨트롤러)는 P2–P3에서 이어집니다.

NPU는 모델을 미리 컴파일하며 모델 × SDK × 장치 × 길이 구간 × 배치 × 옵션 조합마다 결과물이 하나씩 생깁니다. 연구실
RNGD + ATOM 노드에서 Rebellions ATOM용 Llama-3.1-8B 빌드 묶음 하나가 **262 GB**를 차지합니다. 그래프마다 가중치가 통째로
들어 있기 때문입니다. FuriosaAI RNGD는 HF 체크포인트와 바이트가 같은 가중치를 세 벌 보관하고 있었습니다. 내용 기반
분할은 고정 블록으로는 전혀 보이지 않던 ATOM 변형 간 99.3% 공유를 찾아냅니다([측정](docs/ko/artifact-formats.md)).

Onggi(옹기)는 서로 다른 바이트를 한 번만 저장하고, pod가 요청하기 전에 필요한 노드로 옮겨 두며, 어떤 엔진에도 일반
파일로 넘겨줍니다.

<p align="center"><img src="docs/assets/architecture.svg" width="100%" alt="Onggi 구조: 사용자는 Kubernetes API로 조작하고, onggi-controller가 컴파일 팜·프리페치·GC·웹훅을 맡으며, 영속 계층은 교체 가능한 fs·s3·oci 백엔드, 캐시 계층은 노드 agent가 없는 청크를 받아 peer끼리 나누고 엔진 pod에 마운트한다. rm-core, rm-accel, rm-controller, llm-d, npu-train, lmd-top, Optimus Prime과 연결된다"></p>

**흐름:** ① 컴파일 Job(또는 `onggi push`)이 결과물을 청크로 나눠 영속 계층에 없는 청크만 올리고 ② Kubernetes API의 Ref를
커밋합니다. ③ pod가 결과물을 마운트하면 그 노드의 agent가 없는 청크를 peer나 영속 계층에서 받아 모든 해시를 검증한 뒤 엔진에
일반 파일로 넘깁니다. ai-rm·llm-d의 배치 결정과 확장 신호가 들어오면 pod가 생기기 전에 미리 받기 시작합니다.

## 하는 일

| | |
|---|---|
| **한 번만 저장** | 내용 기반 청크, 결과물별 매니페스트, 가중치는 배열 방식별로 한 벌 |
| **미리 배치** | 노드별 캐시 계층, HRW 배치와 peer 간 전송; 배치·확장 신호로 프리페치 |
| **파일로 전달** | CSI 볼륨: kubelet이 준비된 결과물을 마운트; vLLM, vllm-rbln, furiosa-llm, rngd-pd, Optimus Prime 수정 불필요 |
| **선언형 빌드** | `ArtifactSet` CRD가 컴파일 팜을 구동; 게시할 때 중복 제거 |
| **멀티 노드** | 영속 계층은 기존 이레이저 코딩 오브젝트 스토어; 캐시는 노드 장애를 전제로 설계 |

## 빠른 시작

```bash
go build -o onggi ./cmd/onggi            # Go 1.24 이상
./onggi dedup-report --pairs 6 --fixed-block 1MiB <모델 저장소 경로>...
./onggi dedup-report --json <경로>... > report.json
```

경로를 훑어(심볼릭 링크는 따라가지 않음) 파일을 종류별(HF 원본, RNGD 변환 가중치·그래프·묶음, ATOM decode·prefill
`.rbln`)로 정렬해 원본이 먼저 오게 하고, 파일을 병렬로 내용 기반 분할(FastCDC, 기본 16/64/256 KiB)한 뒤 종류별 논리 크기,
고유 바이트, 앞선 종류 대비 새로 추가되는 바이트를 출력합니다. `--fixed-block`은 비교용으로 고정 크기 블록 측정을 함께
하고, `--pairs N`은 가장 큰 N개 파일끼리 서로 얼마나 겹치는지 보여줍니다.

결과물 저장·복원(저장소에 없는 청크만 청크 단위로 압축해 올리고, 복원할 때 모든 청크의 해시를 확인):

```bash
export ONGGI_STORE=/mnt/models/onggi        # 아무 POSIX 마운트: 로컬 디스크, NFS, SMB, CephFS, ...
# 또는 S3 서버(Ceph RGW, SeaweedFS, Garage, VersityGW, 클라우드). 버킷은 미리 만들어 둬야 함
export ONGGI_STORE='s3://models/onggi?endpoint=s3.lab.example:8333&insecure=1' AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
./onggi push --ref llama-3.1-8b@atom/pd <빌드 디렉터리>
./onggi pull llama-3.1-8b@atom/pd <복원 디렉터리>
./onggi ls; ./onggi inspect llama-3.1-8b@atom/pd
./onggi pull --cache /var/lib/onggi/cache llama-3.1-8b@atom/pd-b8 <디렉터리>   # 노드 캐시: 노드에 없는 것만 읽음
```

연구실 노드에서 ATOM Llama-3.1-8B 빌드 묶음 264 GB가 **15.9 GB**로 저장됩니다(push 3분, pull 5분, 원본과 바이트 단위
동일). 2단계 청크 분할이 새 ATOM 청크를 약 4 KiB 하위 청크로 다시 나눠서, 같은 가중치를 다른 순서로 담은 prefill
변형끼리 중복을 찾습니다(새 데이터 46.8 GB → 24.9 GB). 여기에 청크 단위 압축이 1.6배를 더합니다. `--compress none`,
`--sub 0`으로 각각 끌 수 있습니다.

`--cache DIR`를 주면 pull이 노드에 이미 있는 청크는 로컬 캐시 저장소에서 읽고, 새로 받은 청크는 캐시에 더합니다.
연구실 노드에서 ATOM decode 변형 하나(저장소에서 10.2 GB 읽음)를 받은 뒤, 두 번째 변형은 저장소에서 28 MB(0.3%)만,
prefill 빌드는 1.9 GB를 읽었고, 같은 변형을 다시 받을 때는 0 B를 읽었습니다. 모두 원본과 바이트 단위로 같았습니다.
캐시 정리(eviction)는 노드 agent에서 다룹니다.
Kubernetes에서는 CSI 드라이버([deploy/csi](deploy/csi/README.md))가 ref를 읽기 전용 볼륨으로 마운트합니다. 노드는 결과물을
노드 캐시를 거쳐 한 번만 풀어 두고, 같은 ref를 쓰는 pod는 그 사본을 함께 씁니다. 연구실 클러스터에서 두 번째 decode 변형은
저장소에서 28 MB만 읽었습니다. init container로 pod의 emptyDir에 푸는 방법도 있습니다([deploy/k8s](deploy/k8s/README.md)).

정리(GC): `onggi rm-ref`로 ref를 지우면, `onggi gc`가 어떤 ref에서도 닿지 않는 데이터를 회수합니다(ADR-0005).
- 한 번 실행하면 참조되지 않는 객체를 후보로 기록합니다.
- `--grace`(기본 24시간)가 지난 뒤 다시 실행하면, 그때까지도 참조되지 않는 후보를 지웁니다. 단, push가 lease를 잡고 있으면 지우지 않습니다.
- 대부분 죽은 pack은 먼저 재압축(repack)합니다(`--repack 0.25`).

연구실 노드에서 ATOM 전체 묶음과 decode 변형 하나를 지우고 두 변형만 남겼더니, 네 번 실행해 pack이
15.9 GB에서 12.2 GB가 됐습니다(살아 있는 데이터 12.1 GB). 남은 두 결과물은 모두 원본과 바이트 단위로 같았습니다.
노드 캐시에는 ref가 없습니다. `onggi gc --store CACHE --roots-store S --keep-used 168h`를 실행하면 정한 기간 안에
이 캐시로 받은 결과물(pull이 사용 기록을 남김)이 필요로 하는 것만 남기고 나머지를 지웁니다. 연구실 노드에서 변형 세 개를
담은 캐시(12.2 GB)가 아직 쓰는 변형 하나만 남겨 10.2 GB로 줄었고, 그 변형을 다시 받을 때 저장소에서 읽은 양은 0 B였습니다.

메모리:
- pull은 저장소 전체 색인이 아니라 결과물의 locator만 읽습니다. 이 묶음에서 기본 설정은 최대 3.6 GB RSS이고,
  `GOMEMLIMIT=1800MiB`(Go의 소프트 메모리 한도)를 주면 1.9 GB 아래로 유지됩니다. 노드 agent에는 이 한도를 설정하기를 권장합니다.
- push는 중복 제거를 위해 저장소 색인(압축 배열)을 메모리에 들고 있으며, 기본 설정에서 최대 1.9 GiB입니다(push 2분 24초).

## 문서

| | |
|---|---|
| [설계](docs/ko/design.md) | 문제, 목표, 품질 속성, 원칙, 구조, 흐름, 멀티 노드 저장, 로드맵 |
| [결과물 형식](docs/ko/artifact-formats.md) | RNGD·ATOM 결과물의 구성과 공유 비율(실측) |
| [연계](docs/ko/integration.md) | ai-rm 구성요소, lmd-top, llm-d와의 연결 |
| [관련 연구](docs/ko/related-work.md) | 구성요소별 선행 연구, 주장하지 않는 것, 차별점 |
| [ADR](docs/adr/README.md) | 구조 결정 기록 |

## ai-rm 구성요소

Onggi는 이종 AI 자원관리 스택 [ai-rm](https://github.com/ai-computing/ai-rm)의 구성요소이며
[rm-core](https://github.com/ai-computing/rm-core) 계약으로 연결됩니다. 운영 콘솔은 [lmd-top](https://github.com/ai-computing/lmd-top)입니다.

## 기여

[CONTRIBUTING.md](CONTRIBUTING.md)를 참고하세요. 라이선스: [Apache-2.0](LICENSE).
