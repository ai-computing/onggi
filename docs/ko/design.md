# Onggi 설계

상태: **초안 v0.1** (2026-10-21) · [English](../design.md)

Onggi는 Kubernetes + llm-d 환경에서 모델 가중치와 가속기용 컴파일 결과물을 내용 주소(content-addressed) 방식으로
저장하는 시스템입니다. 벤더·버전·컴파일 조합이 달라도 같은 바이트는 한 번만 저장하고, pod가 필요로 하기 전에 해당
노드로 옮겨 두며, 어떤 추론 엔진에도 일반 파일로 넘겨줍니다.

*옹기*는 음식을 담아 저장하고 발효시키는 우리 전통 그릇입니다. 모델을 담으면, 굽기(컴파일)를 거친 결과물이 나옵니다.

## 1. 문제

NPU는 모델을 미리 컴파일합니다. 모델 × 컴파일러/SDK 버전 × 장치 × 병렬화 × 시퀀스 길이 구간 × 배치 × 컴파일 옵션
조합마다 결과물이 따로 생기고, SDK를 올리면 전부 다시 만들어야 합니다. 연구실 RNGD + ATOM 노드 실측(2026-10-21,
상세: [artifact-formats.md](artifact-formats.md)):

| | FuriosaAI RNGD | Rebellions ATOM |
|---|---|---|
| 그래프 | `.edf`, 수 KB~16 MB, 내용 해시로 이름 지정; 노드 전체 531개, 0.28 GB | 각 `.rbln` 안에 포함 |
| 가중치 | HF safetensors를 다시 나눈 것으로 **원본과 바이트가 동일**; 결과물 디렉터리마다 복사본(Llama-3.1-8B 3벌 = 48 GB) | 컴파일러가 배열을 바꿔 **모든 `.rbln`에 통째로 포함** |
| 용량 | 그래프는 작고 가중치가 중복 | `.rbln` 215개, **979 GB**; Llama-3.1-8B 빌드 묶음 하나: 18개, 262 GB |

121 GB 표본(HF 원본, RNGD 변환본, ATOM 파일 6개)에 내용 기반 분할(content-defined chunking)을 적용한 결과:

- RNGD 변환 가중치 ↔ HF 원본: 100% 공유
- ATOM decode 변형(배치 1/2/4, 길이 256/512)끼리: 99.3% 공유
- ATOM decode ↔ ATOM prefill: 27.5% 공유 — 컴파일러가 가중치 배열을 두 가지로 씀
- HF 원본 ↔ ATOM: 0% — 배열 변환 때문에 벤더 간 중복 제거는 불가
- 고정 크기 블록(1 MiB, 4 KiB)으로는 ATOM 배치 변형 사이 공유가 0%로 보임. 같은 바이트가 어긋난 위치에 있기 때문이며,
  내용 기반 경계로만 찾을 수 있음

`onggi dedup-report`(P0)로 전체를 측정한 결과: 262 GB ATOM 빌드 묶음의 고유 바이트는 44.7 GB(5.9배), HF 원본과 RNGD
변환본을 합치면 330 GB → 64.0 GB(5.2배), 노드 모델 저장소 전체는 1.73 TB → 0.94 TB(1.84배). prefill은 chunk 크기마다
가중치 배열이 달라 가장 비싼 축입니다. 그래프는 작고 낭비는 가중치 복사본입니다. 해당 노드 디스크는 80% 사용 중이었습니다.

용량 외에도 같은 결과물이 cold start(받기·조립·적재), 0에서의 확장 지연, 모델을 어느 노드에 둘 수 있는지를 좌우합니다.
지금은 SMB 공유 볼륨, 10분마다 인벤토리 ConfigMap을 만드는 CronJob, lmd-top이 생성하는 컴파일·이동·삭제 Job으로
수작업에 가깝게 관리하고 있습니다.

## 2. 목표와 비목표

목표

1. 서로 다른 바이트(가중치·그래프·메타데이터)는 벤더·SDK 버전·조합에 관계없이 한 번만 저장
2. pod가 필요로 하기 전에 올바른 노드에 결과물 준비; cold start를 측정하고 줄임
3. 모든 엔진(vLLM, vllm-rbln, furiosa-llm, rngd-pd, Optimus Prime)이 일반 파일로 읽음 — 엔진 수정 없음
4. Kubernetes 방식: CRD로 선언, 표준 볼륨 방식으로 전달, Prometheus로 관측
5. 처음부터 멀티 노드: 저장소와 캐시가 여러 노드에 걸치고 노드 장애를 견딤
6. rm-core 계약으로 ai-rm 스택과 연동 ([integration.md](integration.md))

비목표(현재)

- 영속 분산 저장소를 직접 만들지 않음. 영속 계층은 사이트가 쓰는 저장소를 백엔드 인터페이스 하나로 연결(§6, ADR-0008)
- 벤더 형식이나 컴파일러를 바꾸지 않음. 벤더 도구가 만든 것을 그대로 저장
- KV 캐시 저장. 청크 계층으로 prefix KV 블록을 담을 수 있지만 P5 이전에는 범위 밖

## 3. 품질 속성

목표치는 초기값이며 P0–P2 실측으로 확정합니다(ISO/IEC 25010 분류, 절충은 ADR로 기록).

| 속성 | 목표 | 확인 방법 |
|---|---|---|
| 무결성 | 엔진에 넘기는 모든 바이트는 해시 검증; 손상된 청크는 절대 제공하지 않음 | 속성 기반 테스트, 장애 주입 |
| 신뢰성 | 노드 장애·agent 중단으로 커밋된 결과물을 잃지 않음; 읽기는 peer → 영속 계층 순으로 대체 | 카오스 테스트, 결정론적 시뮬레이션 |
| 저장 효율 | 위 ATOM 빌드 묶음에서 5배 이상; RNGD 변환 복사본 → 1벌 | `onggi dedup-report` 실측 |
| Cold start | 노드에 캐시가 있으면 로컬 디스크 읽기 수준; 없으면 OCI pull 기준선 이하 | rm-bench `artifact-coldstart` vs 기준선 |
| 확장성 | 노드가 늘수록 처리량 증가(P2P); 노드 추가·제거 시 캐시 청크의 약 1/N만 이동 | 시뮬레이션, 클러스터 실측 |
| 운영성 | 모든 상태를 CRD와 지표로 확인; 모든 작업은 실행 전 검토 가능 | lmd-top, Grafana |
| 호환성 | OCI distribution·ModelPack 호환 매니페스트; 벤더 형식 무변경 | 상호운용 테스트 |

## 4. 설계 원칙

| 원칙 | 출처 | 적용 |
|---|---|---|
| 품질 속성을 먼저 정하고 절충을 기록 | ISO/IEC 25010, SEI ATAM, ADR(Nygard) | §3 목표, `docs/adr/` |
| 바뀔 가능성이 큰 것을 모듈 경계 뒤에 숨김 | Parnas(1972), Ousterhout *A Philosophy of Software Design* | 벤더 형식은 어댑터 안에만; 코어는 청크와 매니페스트만 앎 |
| 포트와 어댑터, 경계 컨텍스트 | 헥사고날 아키텍처(Cockburn), DDD | 컨텍스트 4개: Catalog, Data, Placement, Build(§5) |
| 불변·내용 주소 데이터 | git, OCI, restic | 캐시 무효화 불필요, 복제본 불일치 불가능 |
| 끝단에서 검증 | End-to-end argument(Saltzer·Reed·Clark, 1984) | 노드에 도착한 바이트를 해시로 검증, 전송 경로는 신뢰하지 않음 |
| 언제든 죽어도 되는 구조, 수준 기반 조정 | Crash-only software(Candea & Fox, 2003), K8s 컨트롤러 | 모든 연산 멱등; 복구 = 재시작 + 재조정 |
| 안정성 패턴 | *Release It!*(Nygard) | 타임아웃, 지터 재시도, 서킷 브레이커, 격벽, 백프레셔 |
| 중앙 인덱스 없는 배치 | Dynamo(2007), Rendezvous/HRW 해싱(1998), CRUSH | 청크 해시와 멤버십으로 소유 노드 계산 |
| 내용 기반 분할 | LBFS(2001), FastCDC(2016) | 어긋난 위치의 중복까지 제거(§1) |
| 함수가 아니라 분산 프로토콜을 검증 | FoundationDB 시뮬레이션, Jepsen, 카오스 엔지니어링 | §11 |
| 차별점만 직접 만듦 | — | 영속 저장·합의·형식은 재사용(§6, ADR-0008) |

## 5. 도메인 모델

경계 컨텍스트 4개, 각각 고유 용어와 담당 구성요소가 있습니다.

| 컨텍스트 | 담당 | 주요 용어 |
|---|---|---|
| **Catalog** | 무엇이 있고 무엇인지 | `ModelSource`, `WeightLayout`, `Artifact`, `Manifest`, `Ref` |
| **Data** | 바이트 | `Chunk`(내용 해시), `Pack`, 영속 계층, 캐시 계층 |
| **Placement** | 바이트가 어디에 있고 어디에 있어야 하는지 | `NodeArtifactCache`, 소유 노드, 프리페치, 축출 |
| **Build** | 결과물 생성 | `CompileRequest`, `Profile`, 컴파일 팜, 어댑터 |

- **ModelSource**: 원본 가중치(HF safetensors) 묶음, 파일 해시로 식별
- **WeightLayout**: 벤더가 가중치를 배열하는 방식. `hf`, `rngd`, `atom-decode`, `atom-prefill` 등. 같은 배열끼리는
  중복 제거되고, 배열이 다르면 안 됨(§1)
- **Artifact**: 컴파일 결과물 하나. **조합 키** =
  `(원본 해시, 장치, 컴파일러+SDK 버전, 배열, 병렬화, 길이 구간, 배치, 옵션)`
- **Manifest**: 결과물의 파일 목록(파일별 청크 순서)과 메타데이터(툴체인, 옵션, 원본). 매니페스트 자체도 내용 주소
  객체이며 그 해시가 결과물의 식별자
- **Ref**: 변경 가능한 이름 → 매니페스트 해시(`llama-3.1-8b@atom/decode-s512-b4`). 유일한 변경 가능 상태이자 GC의 뿌리

## 6. 구조

```
 사용자 ── lmd-top (TUI) · onggi CLI · GitOps YAML
                │ Kubernetes API만 사용
 ┌──────────────▼──────────────── 제어 평면 ──────────────────────────────────┐
 │ CRD: ModelSource · ArtifactSet · CompileRequest · Ref · StorePolicy ·          │
 │      NodeArtifactCache                                                         │
 │ onggi-controller (controller-runtime, 수준 기반)                                │
 │   컴파일 팜 · 프리페치 계획 · GC · compaction · 웹훅(노드 선호도)               │
 └──────────────┬────────────────────────────────────────────────────────────────┘
                │ etcd에는 ref만(작음); 매니페스트·청크는 데이터 평면
 ┌──────────────▼──────────────── 데이터 평면 ────────────────────────────────┐
 │ 영속 계층: 교체 가능한 백엔드(fs: 로컬/NFS/SMB/CephFS/JuiceFS · s3 · oci),      │
 │            영속성은 백엔드가 책임                                              │
 │ 캐시 계층: 노드마다 onggi-agent(DaemonSet, 로컬 NVMe)                          │
 │            청크별 HRW 소유 노드 · peer 간 전송 · 인기 청크 추가 복제            │
 │ Hot:       노드에 조립된 파일, 엔진이 적재                                      │
 └──────────────┬────────────────────────────────────────────────────────────────┘
                │ CSI NodePublishVolume → bind mount (준비될 때까지 pod Pending)
            엔진 pod (vLLM · vllm-rbln · furiosa-llm · rngd-pd · Optimus Prime)
```

| 구성요소 | 언어 | 역할 |
|---|---|---|
| `onggi-controller` | Go | CRD 조정; 컴파일 Job·프리페치·GC·compaction 예약; mutating 웹훅 |
| `onggi-agent` | Go | 노드 캐시, peer용 청크 서버, CSI 노드 플러그인, 조립기, 지표 |
| `onggi` CLI | Go | push, pull, inspect, `dedup-report`, ref 작업 |
| 어댑터 | Python | 벤더 결과물 읽기·쓰기(Furiosa, Rebellions, HF); 컴파일 Job 안에서 실행 |
| `pkg/chunker` | Go | FastCDC + 해시; 유일한 핵심 반복 경로, CI에서 성능 측정 |

영속 계층은 `Backend` 인터페이스(`Put`, `Get`, `Has`, `List`, `Delete`, `Caps`)로만 접근하며 `fs`, `s3`, `oci` 드라이버가 같은
적합성 시험을 통과합니다(ADR-0008).

클러스터에서 도는 것은 모두 Go(controller-runtime, CSI, llm-d Go 템플릿), 벤더 SDK가 있는 곳만 Python입니다(ADR-0006).

## 7. 주요 흐름

**게시**(컴파일 Job 또는 `onggi push`)

1. 어댑터가 결과물을 가중치 파일과 그래프·메타데이터 파일로 나누고, 가중치 파일에 배열 태그를 붙임
2. 모든 파일을 내용 기반으로 분할하고, 영속 계층에 이미 있는 청크 해시를 확인
3. 없는 청크만 올림(객체 수를 줄이도록 큰 단위로 묶음)
4. 매니페스트 기록(내용 주소)
5. `Ref`를 `resourceVersion` 비교-교체로 갱신. **유일한 커밋 지점.** 그 전에 중단되면 참조 없는 청크만 남아 GC가
   치우고, 그 후에 중단되면 완전한 결과물이 이미 게시된 것

**읽기**(pod가 `onggi` 볼륨 `ref: …`를 마운트)

1. kubelet이 agent의 CSI `NodePublishVolume`을 호출; 반환될 때까지 pod는 Pending
2. agent가 ref → 매니페스트를 확인하고, 없는 청크를 로컬 캐시 → 소유 노드(HRW)와 보유를 알린 노드 → 영속 계층 순으로
   요청
3. 도착한 청크는 모두 해시 검증. 로컬 디스크에 파일로 조립(캐시에서 reflink/hardlink)한 뒤 읽기 전용 bind mount.
   엔진은 일반 파일이 필요하며, 지연 읽기(FUSE)는 cold start 전용 선택지(ADR-0004)

**프리페치** — 배치 결정(rm-controller), 확장 신호(llm-d autoscaling, KEDA), `StorePolicy` 고정 설정을 노드별 프리페치
작업으로 바꿔 agent에 전달

**GC** — 뿌리(ref, 고정, 마운트된 볼륨이 잡은 lease)에서 mark-and-sweep, 유예 기간 적용; 영속 계층과 각 캐시가 따로
수행. 핫 경로에서 참조 카운트를 쓰지 않음(ADR-0005)

**Compaction** — *저장* compaction은 청크를 다시 묶고 지난 SDK 버전을 정리; *의미* compaction은 요청 길이 통계로 잘
안 쓰는 조합을 큰 조합으로 흡수하자고 제안하며 패딩 비용을 함께 보여줌(lmd-top에서 확인 후 실행)

## 8. 멀티 노드 저장

영속 계층과 캐시 계층의 역할을 나눠 각각을 단순하게 유지합니다.

| | 영속 계층 | 캐시 계층 |
|---|---|---|
| 보관 | 커밋된 모든 청크·묶음·매니페스트 | 이 노드에서 최근 쓰였거나 프리페치된 청크 |
| 영속성 | 백엔드가 책임(선언: 없음 / 복제 / 이레이저 코딩) | 없음: 잃으면 다시 채움 |
| 배치 | 백엔드 방식 | HRW: 살아 있는 agent 중 `hash(chunk, node)` 상위 R개, 토폴로지 라벨로 분산 |
| 일관성 | 불변 객체; 존재 여부만 중요 | 불변; 해시가 맞으면 유효 |

- **멤버십**: agent마다 `Lease` 보유; 노드 추가·제거 시 약 1/N 청크만 소유 노드가 바뀜(HRW 특성)
- **인기 청크**: 많은 pod가 쓰는 청크(모든 복제본이 쓰는 prefill 배열 등)는 캐시 복제본을 늘리고 peer끼리 퍼뜨려,
  영속 계층은 한 번만 읽힘
- **반엔트로피**: agent가 보유해야 할 청크의 Merkle 요약을 주기적으로 비교해 복구
- **백프레셔**: 노드·링크별 동시성 제한; 서빙 읽기가 프리페치보다 우선(격벽)

장애 처리

| 장애 | 동작 |
|---|---|
| 다운로드 중 agent 중단 | 불완전한 청크는 해시 불일치로 폐기; 재시작 시 매니페스트 기준으로 이어서 |
| 노드 소실 | 해당 캐시 소실; 소유 노드 재계산; 다른 보유 노드나 영속 계층에서 읽음 |
| 영속 계층 노드 소실 | 백엔드의 영속성이 처리(정책으로 등급 요구 가능) |
| 어디서든 손상된 청크 | 해시로 거부; 다른 출처에서 받음; 지표에 기록 |
| 컨트롤러 중단 | 기존 마운트와 읽기는 계속; 복구 전까지 새 게시·GC 없음 |
| 네트워크 분할 | 로컬·도달 가능한 peer로 읽기 계속; lease 상태를 모르는 동안 GC는 삭제하지 않음 |

## 9. Kubernetes·llm-d 연결

확장 지점만 사용하며 Kubernetes나 llm-d를 포크하거나 패치하지 않습니다.

- 모든 상태를 **CRD**로 두어 lmd-top, CLI, GitOps(Argo CD, Flux)가 같은 방식으로 동작
- 전달은 **CSI** 볼륨 `onggi.ai-computing.io`(ephemeral inline); Kubernetes image volume과 init container는 대안(ADR-0004)
- **스케줄링**: onggi 볼륨을 쓰는 pod에 mutating 웹훅이 선호 노드 친화도를 추가(결과물을 가진 노드 선호).
  스케줄러의 ImageLocality와 같은 발상
- **llm-d**: 배포 매니페스트가 onggi 볼륨을 참조; llm-d autoscaling·KEDA 확장 신호로 미리 데우기; npu-train의 LoRA
  어댑터를 동적 적재용으로 제공해 라우터의 어댑터 친화도와 결합; llm-d-benchmark·inference-sim으로 cold start 시험.
  [integration.md](integration.md) 참고
- **지표** `onggi_*`(및 rm-core의 `airm_artifact_*`)를 Prometheus, Grafana, lmd-top에 제공

## 10. 인터페이스(초안)

```yaml
apiVersion: onggi.ai-computing.io/v1alpha1
kind: ArtifactSet
metadata: {name: llama-3.1-8b-atom}
spec:
  source: {modelSource: llama-3.1-8b-instruct}
  device: rebellions-atom
  toolchain: {optimum-rbln: "0.10.3"}
  profiles:
    - {role: decode, sequence: [512, 2048], batch: [1, 8]}
    - {role: prefill, sequence: [512, 2048], chunk: 512}
  placement: {pin: {nodeSelector: {rebellions.ai/atom: "true"}, replicas: 1}}
```

```yaml
volumes:
  - name: model
    csi:
      driver: onggi.ai-computing.io
      readOnly: true
      volumeAttributes: {ref: llama-3.1-8b@atom/decode-s512-b8}
```

CLI: `onggi push <dir> --ref …`, `onggi pull <ref> <dir>`, `onggi inspect <ref>`, `onggi dedup-report <dir>…`,
`onggi ref set|get|rm`.

rm-core 계약(protocol v0.2 제안): `ArtifactStore` — `resolve(ref) → manifest`, `materialize(ref, path)`,
`publish(dir, ref, layout)`, entry-point 그룹 `airm.artifact_stores`.

## 11. 성능·검증 전략

- **처음부터 기준선**: 일반 OCI 이미지 pull, Nydus, Dragonfly, OME model agent, rsync. 지표: cold start(요청 → 엔진
  적재 가능), 조립 시간 p50/p99, 저장 바이트, 노드 장애 후 복구 시간
- **CI 성능 측정**: chunker·packer·조립기; 회귀하면 PR 실패
- **속성 기반 테스트**: 무작위 입력·무작위 위치 이동에서 분할 → 복원이 항상 바이트 동일
- **결정론적 시뮬레이션**: 가상 시계·네트워크·장애로 배치, GC, ref 갱신 재현
- **장애 주입**(연구실 클러스터): 전송 중 agent 종료, 링크 차단, 디스크 가득 참, 영속 청크 삭제
- **퍼징**: 매니페스트와 벤더 형식 파서
- **청크 크기**는 실측으로 결정(ADR-0001): 평균 64 KiB로 어긋난 중복을 찾았고, 가중치 위주 데이터는 1–4 MiB가 인덱스
  면에서 유리할 수 있음

## 12. 보안

내용 주소로 변조를 알 수 있고, 매니페스트는 cosign으로 서명해 출처를 남깁니다. RBAC로 읽기·게시·컴파일·삭제 권한을
나눕니다. CSI 마운트는 읽기 전용입니다. 추적 파일에 자격 증명이나 사이트 주소를 두지 않습니다.

## 13. 로드맵

| 단계 | 산출물 | 완료 조건 |
|---|---|---|
| P0 | `onggi dedup-report`; 기존 저장소 구조(`hub/`, `compiled/`) 가져오기 | §1 수치를 명령으로 재현 |
| P1 | 백엔드 인터페이스 + `fs`/`s3` 드라이버 + 적합성 시험, CLI push/pull | 262 GB ATOM 묶음을 약 45 GB에 저장, pull 파일 바이트 동일 |
| P2 | agent + CSI 볼륨 + 조립기 | 엔진이 onggi 볼륨으로 서빙; 기준선 대비 cold start 측정 |
| P3 | CRD, 컨트롤러, 컴파일 팜, GC | `ArtifactSet`만으로 결과물이 자동 생성 |
| P4 | 프리페치, 웹훅 배치, llm-d autoscaling 연동 | 0에서의 확장 cold start 감소 측정 |
| P5 | 의미 compaction; MoE 전문가 단위 전송; KV 블록 저장 검토 | 실제 트레이스로 절충 측정 |

## 14. 열린 질문

1. ATOM 툴체인에 조합 간 가중치를 공유하는 공식 방법이 있는가(벤더 방식 빌드 하나는 prefill 15 GB + decoder 1 GB)?
   있다면 중복 제거보다 낫다
2. 가중치 위주 데이터의 청크·묶음 크기(ADR-0001)
3. 연구실 영속 계층: 기존 SMB 공유(`fs` 드라이버)로 시작, 이후 Ceph RGW나 SeaweedFS 같은 S3 서버(ADR-0008, MinIO 커뮤니티판은 보관 처리됨)
4. OME의 모델 CRD와 agent를 어디까지 재사용할 수 있는가

## 참고문헌

Parnas 1972 · Saltzer, Reed, Clark 1984 · Thaler & Ravishankar 1998 · Muthitacharoen et al. 2001(LBFS) ·
Candea & Fox 2003 · Weil et al. 2006(CRUSH) · DeCandia et al. 2007(Dynamo) · Nygard, *Release It!* ·
Xia et al. 2016(FastCDC) · Ousterhout, *A Philosophy of Software Design* · ISO/IEC 25010 · SEI ATAM ·
Kubernetes CSI · OCI distribution · CNCF ModelPack · OME · Nydus · Dragonfly · Fluid · llm-d
