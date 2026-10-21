# ai-rm, lmd-top, llm-d 연계

상태: 초안 v0.1 (2026-10-21) · [English](../integration.md)

Onggi는 다른 구성요소와 마찬가지로 [rm-core](https://github.com/ai-computing/rm-core) 계약으로
[ai-rm](https://github.com/ai-computing/ai-rm) 스택에 붙습니다. 구성요소끼리 서로 직접 가져다 쓰지 않습니다. 엔진 내부
([aicomp](https://github.com/ai-computing/aicomp)의 Optimus Prime, FASOP)는 해당 팀만 수정하고, onggi는 이슈로 요청합니다.

## 저장소별

| 계층 | 저장소 | Onggi가 주는 것 | Onggi가 받는 것 | 작업 |
|---|---|---|---|---|
| 계약 | rm-core | — | `ArtifactStore` 계약, 매니페스트 스키마, `airm_artifact_*` 지표 이름 | rm-core PROTOCOL v0.2 절 + 적합성 검사 |
| L1 | rm-accel | 노드별 보유 현황(`NodeArtifactCache`) | 장치·SDK·컴파일러·펌웨어 버전 → 호환 결과물 키 | AcceleratorProfile 필드 |
| L2 | hetero-npu-p2p | — | — | 이후: host → NPU 가중치 적재에 DMA-BUF |
| L2 | npu-offload | MoE 가중치를 전문가 단위로 전송(cold 전문가는 저장소에서) | 전문가 적중 통계 → hot/cold 힌트 | 전문가 단위 매니페스트, 저장소 기반 로더 |
| L3 | rngd-pd | 조합별 컴파일 그래프·가중치 마운트 | 필요한 조합(S, B, TP) | 고정 경로 대신 onggi 볼륨으로 로드(첫 소비자) |
| L3 | npu-train | 원본 가중치, 컴파일 캐시 | 학습한 어댑터·체크포인트 → 새 `ModelSource` | 학습 종료 시 게시 |
| L3 | Optimus Prime · FASOP | 엔진용 단일 결과물 인터페이스 | FASOP 비용 모델 → 컴파일할 조합 | **aicomp 기능 요청만** |
| L4 | rm-controller | 배치 비용에 결과물 준비 상태 | 배치 결정(`airm-placement`) → 프리페치 | 준비 상태 항, 프리페치 연결 |
| L4 | hetero-llm-d | 공유 모델 저장소 대체 | 확장 신호 → 미리 데우기 | 매니페스트를 onggi 볼륨으로 |
| L5 | rm-bench | — | `artifact-coldstart` 시나리오 | 새 시나리오 |
| L5 | lmd-top | Deploy 화면의 데이터와 작업 | — | 아래 참고 |
| 허브 | ai-rm | — | 버전 고정, 통합 차트, 계층 지도 | 서브모듈, 차트, 문서 |
| 분석 | 트레이스 분석 | — | 요청 길이 분포 → 남길 조합 | 의미 compaction 입력 |

## lmd-top

lmd-top의 Deploy 화면이 이미 초기 형태를 하고 있습니다: Library(저장소 빌드, 결과물에 기록된 툴체인, 컴파일·프리페치·배포,
이동·삭제·재스캔을 검토 가능한 Job으로), Zoo(벤더 모델 목록), 컴파일 어드바이저(`npu-compat.json`, 잘못된 옵션 예측, 컴파일
이력), 10분마다 `model-inventory` ConfigMap을 만드는 탐색 CronJob. onggi가 들어오면 **onggi는 진실의 원천이자 실행자,
lmd-top은 운영 콘솔**이 됩니다.

| 현재 lmd-top | onggi 도입 후 | lmd-top 역할 |
|---|---|---|
| 탐색 CronJob → `model-inventory` ConfigMap | 보유 현황이 CRD에 실시간 | Library가 CRD를 읽음(10분 지연 없음); 전환 기간 동안 onggi가 ConfigMap도 계속 발행 |
| 컴파일 Job 직접 생성 | `CompileRequest` / `ArtifactSet` → 컴파일 팜 → 분할 게시 | 사전 점검·진행률; 결과는 자동 중복 제거 |
| `mv`/`rm -rf` Job으로 이동·삭제 | ref 작업 + GC(불변 데이터, 사용 중 결과물 자동 보호) | 더 안전한 작업, 회수 공간 미리 보기 |
| 공유 저장소로 프리페치 | 노드별 프리페치, peer 간, 없는 청크만 | 노드별 프리페치 진행 표시 |
| probe Job으로 출처 확인 | 매니페스트에 툴체인·옵션·원본 해시 기록 | 매니페스트에서 바로 읽음 |
| `npu-compat.json`, 옵션 예측, 이력 | onggi 컴파일 계획기로 이전 | 어드바이저 UI는 유지, 데이터는 onggi에서 |

추가할 화면: 빌드별 논리·실제 크기와 공유 내역, 노드 캐시 지도(노드별 hot/warm), cold start 타임라인(조립 → 적재 → 준비),
compaction 제안(회수 공간과 패딩 비용).

## llm-d

| llm-d 구성요소 | 연결 | 효과 |
|---|---|---|
| 배포(ModelService 차트, hetero-llm-d 매니페스트) | pod 템플릿이 `onggi` 볼륨을 마운트 | 모든 엔진에 같은 전달 경로 |
| llm-d-autoscaling, KEDA | 확장 신호 → 대상 노드 미리 데우기; onggi가 노드별 준비 비용 제공 | 0에서의 확장 cold start 감소 |
| llm-d-router / EPP(InferencePool) | LoRA 어댑터를 동적 적재용으로 제공해 어댑터 친화도와 결합; 확장 시 준비 상태 반영 | 요청 단위 라우팅보다 어댑터·확장에서 효과 |
| llm-d-kv-cache | 이후: prefix KV 블록을 내용 주소 청크 계층에 | 영속 prefix 저장소(검토) |
| llm-d-benchmark, inference-sim | cold start 벤치마크, NPU 없이 컨트롤러 시험 | CI에서 검증 |
| llm-d-go-template | onggi Go 서비스가 이를 따름 | 처음부터 상위 기여 가능한 형태 |

원칙: 확장 지점(CRD, CSI, 웹훅, InferencePool/EPP 플러그인, 지표)만 사용. Kubernetes와 llm-d는 포크·패치하지 않음.

## 순서

| 단계 | Onggi | lmd-top | llm-d / ai-rm |
|---|---|---|---|
| P0 | dedup-report, 기존 저장소 구조 가져오기 | 용량·중복 화면 | — |
| P1 | 청크 저장소 + CLI | Library가 ConfigMap과 함께 CRD도 읽음 | rm-core `ArtifactStore` 초안 |
| P2 | agent + CSI 볼륨 | 노드 캐시 지도, cold start 타임라인 | rngd-pd 로더를 onggi 볼륨으로 |
| P3 | CRD, 컴파일 팜, 컴파일 계획기 | 컴파일·프리페치가 CR 생성 | rm-accel 호환 키, npu-train 게시 |
| P4 | 프리페치·배치 | — | rm-controller 프리페치, autoscaling 미리 데우기, 어댑터 제공, rm-bench 시나리오 |
| P5 | 의미 compaction, 전문가 단위 전송 | compaction 제안 | npu-offload 전문가 전송, kv-cache 검토, aicomp 요청 |
