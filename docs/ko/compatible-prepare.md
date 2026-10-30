# 호환 결과물 선택과 파일 사전 준비

[English](../compatible-prepare.md)

P3의 첫 구현입니다. `onggi prepare`는 지정한 후보에서 조건이 정확히 일치하는
결과물을 골라 엔진 시작 전에 파일을 준비합니다. 기존 POSIX/S3, 노드 청크 캐시,
peer 경로를 재사용합니다. 자동 컴파일·SDK 탐색·NPU 그래프 예열은 포함하지 않습니다.

```sh
onggi push --store "$STORE" --profile profile.json --ref llama/rngd/b4-s256 ./build
onggi prepare --store "$STORE" --request request.json --plan-only \
  llama/rngd/b1-s256 llama/rngd/b4-s256
onggi prepare --store "$STORE" --request request.json \
  --cache /var/lib/onggi/cache --dir /models/ready \
  llama/rngd/b1-s256 llama/rngd/b4-s256
```

`profile.json`과 `request.json`은 동일한 `onggi.profile.v1` 구조입니다.
[전체 예제와 필드 설명](../compatible-prepare.md#publish-an-explicit-profile)을 참고하세요.
모델 원본·빌드 옵션의 정규 SHA256 해시, 장치, 런타임 빌드, SDK·컴파일러·펌웨어,
파일 형식, 가중치 배치 방식, 토폴로지, TP, 배치, 시퀀스 길이를 모두 명시해야 합니다.
원본 해시는 변경 불가능한 체크포인트 파일·해시 목록을 식별하며, 옵션 해시는 게시자와
소비자가 합의한 정확한 옵션 기록 바이트의 해시입니다. 예제의 해시·빌드 식별자는
실제 값으로 바꿔야 합니다. ATOM은 실제 ATOM 장치·rebel·펌웨어·런타임, rbln 형식,
가중치 배치와 P/N 토폴로지를 사용하며 RNGD 프로필을 공유하지 않습니다.

어느 필드든 다르면 제외하며 이유를 JSON으로 반환합니다. 큰 배치나 길이의 그래프에
패딩할 수 있다고 추측하지 않습니다. 누락·오타 필드, 빈 값·unknown, 0/비정규 해시,
양수가 아닌 차원은 거부합니다. 선언은 매니페스트 해시로 파일에 결합되지만, 실제
정확성은 게시자가 책임집니다. 벤더 ABI 인증이나 서명된 보증은 아닙니다.
기존 프로필 없는 결과물은 일반 push/pull은 되며 자동 선택에서만 제외됩니다.

여러 후보가 일치하면 매니페스트 해시의 사전순으로 고릅니다. 후보 ref는 GC lease
안에서 한 번만 해석하며, 이후 ref가 바뀌어도 선택한 해시를 복원합니다. 없는 결과물이나
손상된 매니페스트를 조용히 건너뛰지는 않습니다. 재현 가능한 작업에는 해시를 넘기세요.

표준 출력은 JSON이며 `selection`에 선택·제외 이유, `ready`에 준비 결과가 담깁니다.
종료 코드 0은 성공, 1은 일치 후보 없음/운영 오류, 2는 잘못된 입력입니다.
`--plan-only`는 `ready:false`로 선택만 확인하며 모델 청크·대상 폴더·캐시를 준비하지
않습니다. 백엔드를 여는 동작은 기존 CLI와 같아 빈 저장소를 초기화할 수 있습니다.

대상 경로는 없어야 합니다. 형제 임시 디렉터리에서 청크 해시를 검증하고 전체 파일이
완성된 뒤 rename으로 게시합니다. `<대상>.onggi-lock`으로 같은 대상을 준비하는
프로세스를 막습니다. 일반 오류·취소에는 자신이 만든 임시 폴더·잠금만 정리하고
캐시 청크는 남깁니다. 기존 대상·심볼릭 링크·다른 작업의 잠금은 거부합니다.
프로세스가 강제 종료되면 잠금·임시 파일이 남을 수 있으므로 소유 프로세스가 없는지
확인한 뒤 정리하거나 새 경로를 사용하세요. 기존 디렉터리 자동 채택 및 크래시 후
디스크 영속성 보장은 없습니다. 파일 권한은 원본을 유지하므로 엔진 UID/그룹을 맞추고,
CSI가 관리하는 루트와 준비 디렉터리를 공유하지 마세요.

[init-container 예제](../../deploy/k8s/prepare-initcontainer.yaml)는 이 명령 성공 후
엔진을 시작합니다. `ready:true`는 **검증된 파일 준비 완료**이며 NPU 서빙 준비와
다릅니다. 엔진의 SDK·그래프 검증/적재·readiness는 이어서 수행해야 합니다.
클러스터 후보 탐색, CRD·컴파일 팜, 배치·확장 신호 연동과 NPU 예열은 후속 작업입니다.
