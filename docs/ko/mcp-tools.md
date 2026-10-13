# Kenfold MCP 도구

[English](../mcp-tools.md) | [한국어](mcp-tools.md)

Kenfold가 MCP로 제공하는 도구의 계약과 동작 설명

스키마는 [`internal/mcpserver/server.go`](../../internal/mcpserver/server.go)의 Go 타입에서 생성하며 해당 파일을 명세의 기준으로 사용

## 공통 규칙

- **`project`:** 현재 저장소의 Git 원격 URL, 원격 저장소가 없으면 자유 형식의 프로젝트 이름 사용 가능
  - 소문자 `host/path`로 정규화, 스킴과 인증정보, 포트, 쿼리, 끝의 `/`와 `.git` 제거
  - SCP 형식인 `git@host:owner/repo`는 `host/owner/repo`로 변환
  - `https://github.com/Org/Repo.git`과 `git@github.com:org/repo`는 모두 `project:github.com/org/repo`로 처리
  - 생략하면 사용자 전체 메모리(`scope = user`), 프로젝트별 조회에도 항상 포함
- **메모리 유형:** `semantic`, `episodic`, `project`, `preference`, `codebase`, `temporary`, [메모리 모델](../adr/0001-architecture.md#memory-model)(영문) 참고
- **신뢰도:** `user`는 사용자 확인, `agent`는 에이전트 작성, `external`은 외부 콘텐츠에서 유래한 메모리로 취급 시 주의 필요
- **상태:** `proposed`, `active`, `superseded`, `deleted`
  - 활성 상태이며 만료되지 않은 메모리만 조회
  - 소유자가 중복이나 모순 정리, 세션 요약 제안을 적용해도 기존 메모리를 `superseded`로 변경
  - [정리 설계](../adr/0004-consolidation.md)(영문) 참고
- **호출 에이전트:** `source_agent`는 도구 인자가 아니라 서버가 결정
  - HTTP는 API 키의 에이전트 이름 또는 OAuth 승인 시 소유자가 지정한 이름 사용
  - stdio는 `KENFOLD_AGENT` 사용, 없으면 클라이언트의 `clientInfo.name`을 소문자로 정리해 사용
  - 아무 정보도 없으면 `unknown`
- **접근 권한:** API 키는 읽기와 쓰기 허용, OAuth는 `memory:read` 또는 읽기를 포함하는 `memory:write` 사용
  - 읽기 전용이어도 모든 도구 표시, `remember`, `handoff`, `resume`, `forget` 호출은 권한 오류 반환
  - `resume`은 인계 수신자를 기록하므로 쓰기로 분류
  - `get_context`와 `recall`은 읽기 전용으로 표시, [원격 접속](deploy.md) 참고
- **오류:** 잘못된 인자는 핸들러 실행 전 스키마 검증으로 거절
  - 잘못된 ID, 너무 긴 입력, 이미 교체된 메모리의 재교체는 설명이 포함된 도구 오류(`isError: true`) 반환
  - 내부 실패는 일반적인 오류만 반환하고 상세 내용은 서버 로그에 기록
- **비밀정보:** `remember`와 `handoff`는 API 키, 토큰, 개인키, JWT, URL 속 비밀번호, 비밀 필드의 무작위 형태 값 거절
  - 오류에는 비밀정보의 종류만 표시하고 값은 노출하지 않음
  - `forget` 사유와 검색 쿼리는 거절 대신 비밀정보 가림
- **세션:** 호출의 `_meta`에 `{"kenfold/session_id": "<id>"}`를 넣으면 `source_session`으로 기록, `kenfold hook`의 세션 요약에도 사용
- **로컬 경로:** `/home/me/repo`, `~/repo` 같은 경로는 기기마다 프로젝트가 달라지므로 `project` 값으로 사용 불가
- **메모리는 데이터:** 반환된 `content`를 에이전트에 대한 지시로 해석 금지

## 검색

`recall`과 `get_context`의 `relevant` 영역은 아래 단계로 순위 계산

자세한 내용은 [검색 설계](../adr/0002-retrieval-and-code-refs.md)(영문) 참고

1. **하이브리드 1차 검색**
   - 전문 검색은 쿼리 단어를 OR로 결합하고 `ts_rank`로 정렬
   - PostgreSQL의 `simple` 설정으로 어간 추출 없이 한국어와 영어 혼합 텍스트 처리
   - 임베딩 모델이 있으면 같은 모델로 생성한 벡터의 코사인 유사도 계산
   - `KENFOLD_SEARCH_MAX_DISTANCE`(기본 0.55)보다 먼 결과 제외
   - 두 순위를 Reciprocal Rank Fusion(`k = 60`)으로 결합해 후보 30개 구성
2. **연결 확장**
   - 재순위화 모델이 있으면 상위 8개 후보에서 한 단계 연결된 메모리 추가
   - 세션 요약에서 추출 사실로, 사실에서 원본 요약과 같은 세션의 다른 사실로 확장
3. **재순위화**
   - `KENFOLD_RERANK_URL` 설정 시 교차 인코더로 상위 15개 후보와 쿼리 평가
   - 실패하거나 4초를 넘기면 연결 확장 결과 없이 1차 순위 유지
4. **추가 신호**
   - 참조 파일이나 심볼이 사라진 메모리는 점수의 5%, 참조 심볼이 바뀐 메모리는 60% 유지
   - "last time", "latest", "지난번", "최근"처럼 최근 작업을 묻는 쿼리는 모든 유형의 최신 메모리 우선, 반감기 7일
   - 재순위화 모델이 있으면 세션 요약은 나이에 따라 점수가 최대 40% 감소, 반감기 30일

연결 확장과 일반적인 세션 요약의 시간 감쇠는 재순위화 점수에 맞춘 보정

내부 평가에서 1차 점수에 적용하면 순서를 지나치게 바꿨으므로 재순위화가 있을 때만 적용

`score` 범위는 `(0, 1]`

- 재순위화 사용 시 추가 신호 적용 후의 관련성 확률
- 재순위화 미사용 시 결합 점수, 1.0은 모든 순위 계산에서 1위라는 의미

임베딩 API가 실패하거나 10초를 초과하면 전문 검색으로 호출 완료

그동안 임베딩 없이 저장된 메모리는 백그라운드에서 보충

## 코드 참조

프로젝트 범위의 `project`, `codebase`, `semantic` 메모리를 저장하거나 추출할 때 언급된 파일과 심볼 기록

예: "`RequireAPIKey` in internal/auth/middleware.go hashes the token"

- 경로는 알려진 소스 확장자 또는 `Makefile`, `Dockerfile` 같은 파일명 사용, URL과 절대 경로 및 `Next.js` 같은 제품명은 제외
- 심볼은 백틱이나 호출 형태(`Name()`) 우선, 파일 하나만 언급했을 때는 `CamelCase`, `snake_case` 같은 식별자도 탐색

서버는 저장소를 읽지 않음

저장소 옆에서 실행하는 `kenfold hook`이 세션 시작 시 2초 이내로 검사하거나 `kenfold refs sync`로 직접 검사

Git의 `post-commit` 훅에서도 실행 가능

클라이언트는 `HEAD`의 파일 blob ID 또는 tree-sitter로 찾은 심볼 정의의 공백 차이를 무시하는 해시를 기준값으로 기록

이후 비교 시 아래 규칙 적용

- 참조 파일에 미커밋 변경이 있으면 기준값 고정 보류
- 현재 `HEAD`가 기준 커밋을 포함할 때만 변경 보고
- 오래되었거나 갈라진 체크아웃은 변경의 증거에서 제외

`code_refs[].state`는 `pending`(기준 없음), `current`, `changed`, `missing`(파일이나 심볼 없음) 표시

파일이나 심볼이 사라지거나 심볼이 바뀌면 `stale` 적용

파일 자체의 변경만으로는 오래된 메모리로 판정하지 않음

오래된 메모리는 검색 순위를 낮추고 `get_context`의 프로젝트 지식에서 뒤로 배치하며 세션 훅에도 표시

`kenfold refs status`와 `kenfold memory list --stale`로 검토 가능

## 도구

### `get_context` (읽기 전용)

작업 시작 시 토큰 예산 내에서 필요한 메모리 조회

| 인자 | 유형 | 필수 | 설명 |
|---|---|---|---|
| `task` | string | | 수행할 작업, `relevant` 검색에 사용 |
| `project` | string | | 공통 규칙 참고 |
| `budget_tokens` | integer | | 기본 2000, 200~32000 범위로 제한 |

반환 우선순위

1. `preferences[]`: 사용자 전체 및 프로젝트의 활성 선호
2. `handoff`: 아직 아무도 이어받지 않은 프로젝트의 최신 인계
3. `recent[]`: 훅이 작성한 최근 세션 요약 3개, 각각 최대 800자
4. `project[]`: 활성 프로젝트 메모리, 오래된 코드 관련 메모리는 뒤로 배치
5. `relevant[]`: `task`와 관련된 검색 결과

각 메모리는 한 번만 포함, 예산을 초과하는 메모리는 제외하고 `truncated` 설정

추가 조회에는 `recall` 사용, 토큰 수는 추정값

### `remember` (멱등 저장)

오래 유지할 메모리 저장

| 인자 | 유형 | 필수 | 설명 |
|---|---|---|---|
| `content` | string | 예 | 독립적으로 이해할 수 있는 내용, 최대 8000자 |
| `type` | enum | | 생략 시 교체 대상의 유형, 설정된 모델의 분류, 기본 유형 순으로 결정 (기본 유형은 프로젝트가 있으면 `project`, 없으면 `semantic`) |
| `project` | string | | 생략하면 사용자 전체 범위 |
| `supersedes` | string (id) | | 같은 범위에서 교체할 활성 메모리 |
| `ttl_seconds` | integer | | `temporary` 유형에 필수, 최대 1년 |

반환값: `id`, `type`, `status`, `deduplicated`, `similar`

- **중복 제거:** 같은 유형과 범위의 활성 또는 제안 메모리와 대소문자, 공백, 끝 문장부호만 다르면 `deduplicated: true`와 기존 메모리 반환, 같은 내용으로 교체 요청 시에도 변경 없음
- **유사 메모리:** 저장 후 같은 범위의 유사한 활성 메모리 최대 3개를 `similar[]`로 반환
  - 임베딩 코사인 거리 0.25 이하 또는 trigram 유사도 0.5 이상
  - 어느 내용이 맞는지는 자동 판정하지 않고 에이전트가 교체하거나 삭제
  - 세션 요약과 임시 메모리는 검사 제외
- **교체:** 같은 범위의 활성 메모리만 대상으로 허용
  - 새 메모리가 활성이면 같은 트랜잭션에서 기존 메모리를 `superseded`로 변경하고 이력 보존
  - 새 메모리가 `proposed`이면 승인할 때까지 기존 메모리를 활성으로 유지
- **세션 요약:** 세션 ID와 함께 보낸 `episodic` 메모리는 같은 에이전트와 세션의 이전 요약 교체
- **검토:** 새 `preference`는 `proposed`로 저장, `kenfold memory approve <id>`로 승인한 뒤 제공하고 `trust = user` 적용

### `recall` (읽기 전용)

메모리를 자연어로 검색

| 인자 | 유형 | 필수 | 설명 |
|---|---|---|---|
| `query` | string | 예 | 최대 2000자로 잘라 사용 |
| `types` | enum[] | | 메모리 유형 필터 |
| `project` | string | | 사용자 전체 메모리는 항상 포함 |
| `limit` | integer | | 기본 10, 최대 50 |

`score` 순으로 정렬한 `memories[]` 반환

로컬 CPU 재순위화 사용 시 약 1~2초, 재순위화가 없는 하이브리드 검색은 약 25 ms 소요

측정 조건은 [검색 품질](../../README.ko.md#검색-품질) 참고

### `handoff`

다른 에이전트나 세션이 미완료 작업을 이어가도록 `temporary` 유형의 메모 저장

| 인자 | 유형 | 필수 | 설명 |
|---|---|---|---|
| `summary` | string | 예 | 수행한 작업과 현재 상태, 최대 8000자 |
| `next_steps` | string[] | | 최대 20개, 각각 최대 1000자 |
| `project` | string | | 프로젝트 |
| `ttl_seconds` | integer | | 기본 7일, 최대 30일 |

`id`, `expires_at` 반환

프로젝트마다 활성 인계는 하나이며, 새 인계는 기존 인계를 교체하고 `supersedes`로 연결

### `resume` (멱등 쓰기)

| 인자 | 유형 | 필수 |
|---|---|---|
| `project` | string | |

- 프로젝트의 만료되지 않은 최신 인계를 `next_steps`와 함께 반환
- 인계가 없으면 `handoff` 필드 생략
- 첫 호출은 `resumed_by`와 수신 시간 기록
- 이후 호출은 기록 변경 없이 같은 인계 반환

이어받은 인계는 `get_context`에서 제외, 만료되거나 교체될 때까지 `resume`으로 조회 가능

### `forget` (파괴적 작업, 멱등)

`status = deleted`로 논리 삭제, 행은 감사와 이력 확인용으로 보존하고 조회 결과에서 제외

| 인자 | 유형 | 필수 | 설명 |
|---|---|---|---|
| `id` | string | 예 | 메모리 UUID |
| `reason` | string | | 최대 1000자로 잘라 사용 |

`id`, `status` 반환

첫 삭제 시 `forgotten_by`, `forget_reason` 기록, 다시 삭제해도 기록 변경 없음

## 세션 훅

`kenfold hook`은 Claude Code와 Codex의 명령형 훅으로 에이전트의 API 키로 인증하는 MCP 클라이언트

쓰기에는 다른 클라이언트와 같은 검사 적용

| 이벤트 | 동작 |
|---|---|
| `SessionStart` | `cwd`의 Git 원격 URL로 프로젝트 식별, 이전 세션의 대기 요약 전송, `get_context`를 예산 1500으로 호출해 참고 데이터로 표시한 `additionalContext` 반환, 실패 시 `systemMessage` 경고 후 세션 계속 |
| `UserPromptSubmit`, `Stop`, `PostCompact` | 요청과 최종 응답 및 압축 요약을 로컬 로그에 추가, 비밀정보 가림, 파일 권한 0600, 네트워크 호출 없음 |
| `SessionEnd` | 요청과 최종 응답 및 압축 요약에서 세션 요약을 구성하고 세션 ID를 포함한 `episodic` 메모리로 저장, 먼저 로컬 전송 대기 파일에 기록해 중단이나 서버 장애 시 다음 `SessionStart`에서 재전송 |

Git 저장소 밖의 세션은 기록하지 않고 사용자 전체 컨텍스트만 조회

훅은 항상 종료 코드 0 반환, 설치는 [한국어 README](../../README.ko.md#세션-훅) 참고

## 모델 기반 추출

`KENFOLD_CHAT_URL` 설정 시 `kenfold serve`가 작성 후 수 분이 지난 세션 요약에서 오래 유지할 메모리 추출

저장 전 검사 항목

- 프로젝트 규칙(`project`), 코드 사실(`codebase`), 사용자 선호(`preference`), 일반 사실(`semantic`)만 허용
- 작업, 상태 보고, 붙여 넣은 문서 내용은 제외
- 저장소 밖의 세션에서는 선호와 일반 사실만 추출
- 근거 인용문은 원본 세션에 존재하고 긴 인용이나 코드 블록 밖에 위치해야 함
- 내용은 8~400자, 비밀정보 포함 불가
- 같은 범위의 모든 상태에서 동일 내용이 없어야 하며 거절한 메모리도 중복 검사에 포함

작성자는 `kenfold-extractor`, 저장 항목은 아래와 같음

- 원본의 `source_session`, `attrs.session_agent`
- 근거 인용문인 `attrs.evidence`
- 모델명인 `attrs.extractor_model`
- 유사 메모리 ID인 `attrs.similar_to`
- 원본 요약을 가리키는 `derived_from` 연결

기본 상태는 `proposed`, `kenfold memory review` 등으로 승인한 뒤 제공

`KENFOLD_EXTRACT_POLICY=auto`에서는 명시적으로 서술되었고 기존 메모리와 유사하지 않은 결과 활성화

사용자 선호와 `similar_to`가 있는 결과는 계속 검토 대상으로 유지

같은 모델로 `remember`의 생략된 유형도 분류

`attrs.type_source`는 `agent`, `model`, `superseded`(교체 대상의 유형 사용), `default`(모델 미설정이나 실패 또는 15초 초과로 기본 유형 사용)

## `MemoryView`

```json
{
  "id": "0199...",
  "type": "project",
  "scope": "project:github.com/kenfold/kenfold",
  "content": "HTTP 전송은 무상태 방식, Mcp-Session-Id에 의존하지 않음",
  "source_agent": "claude-code",
  "trust": "agent",
  "created_at": "2026-09-27T06:00:00Z",
  "score": 0.83
}
```

선택 필드

- `expires_at`: 임시 메모리와 인계의 만료 시각
- `next_steps`, `resumed_by`: 인계 정보
- `score`: 검색 결과 점수
- `code_refs`: 참조 파일과 심볼, `state`, `checked_commit`
- `stale`: 코드 참조 기준으로 오래된 메모리 여부

## REST API

비에이전트 클라이언트는 `/api/v1` 사용

`/mcp`와 같은 Host 허용 목록, 교차 사이트 요청 차단, API 키 인증 적용

오류 형식은 `{"error": "..."}`

`POST /api/v1/refs/check`는 쓰기 권한 필요, 읽기 전용 OAuth 클라이언트에는 403 반환

| 엔드포인트 | 용도 |
|---|---|
| `GET /api/v1/refs?project=P` | 프로젝트 `P`의 검사 대상, `{"scope", "targets": [{"path", "symbol", "anchor_commit", "anchor_hash"}]}` 형태로 기준값이 없는 항목부터 최대 500개 반환 |
| `POST /api/v1/refs/check` | `{"project", "commit", "results": [{"path", "symbol", "anchor_commit", "found", "hash", "resolved_path"}]}` 보고, 결과 최대 1000개, 처리 후 상태별 개수 반환 |

- `anchor_commit`이 없으면 `commit`을 기준으로 참조 고정
- `found: false`이면 `unresolved`로 표시하고 다음에 다시 제공
- `anchor_commit`이 있으면 해당 커밋에서 고정한 해시와 비교

## OAuth 인증

`KENFOLD_PUBLIC_URL` 설정 시 `/mcp`와 `/api/v1`에서 OAuth 액세스 토큰도 허용

유효한 토큰이 없으면 401과 아래 헤더 반환

```text
WWW-Authenticate: Bearer resource_metadata="<public URL>/.well-known/oauth-protected-resource/mcp"
```

| 엔드포인트 | 용도 |
|---|---|
| `GET /.well-known/oauth-protected-resource/mcp` | 보호 리소스 메타데이터(RFC 9728), 리소스 `<public URL>/mcp`, 인증 서버, `memory:read`와 `memory:write` 범위 |
| `GET /.well-known/oauth-authorization-server` | 인증 서버 메타데이터(RFC 8414), `/.well-known/openid-configuration`에서도 제공 |
| `GET, POST /oauth/authorize` | PKCE S256 인증 코드 흐름과 동의 페이지, 응답에 `iss` 포함(RFC 9207) |
| `POST /oauth/token` | `authorization_code`, `refresh_token` 교환, 클라이언트 인증은 `none` 또는 `private_key_jwt` |
| `POST /oauth/register` | 공개 클라이언트의 동적 등록(RFC 7591), `KENFOLD_OAUTH_DCR=false`로 비활성화 |
| `POST /oauth/revoke` | 토큰 철회(RFC 7009) |

- **클라이언트 식별:** HTTPS URL인 `client_id`는 클라이언트 메타데이터 문서를 가리키며 SSRF 방어를 적용해 조회
- **리소스 지정:** `<public URL>/mcp`만 허용, 토큰은 이 리소스에 연결
- **토큰 수명:** 액세스 토큰 1시간, 갱신 토큰 30일, 갱신 토큰은 사용할 때마다 교체
- **토큰 재사용:** 이미 교체된 갱신 토큰이나 사용한 인증 코드를 다시 제출하면 해당 클라이언트의 승인 철회
