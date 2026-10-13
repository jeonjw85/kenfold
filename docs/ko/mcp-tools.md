# Kenfold MCP 도구

[English](../mcp-tools.md) | [한국어](mcp-tools.md)

Kenfold가 MCP로 제공하는 도구의 계약입니다. 스키마는 [`internal/mcpserver/server.go`](../../internal/mcpserver/server.go)의 Go 타입에서 생성합니다. 해당 파일이 명세의 기준이며, 이 문서는 의도와 동작을 설명합니다.

## 공통 규칙

- **`project`:** 현재 저장소의 Git 원격 URL입니다. 원격 저장소가 없으면 자유 형식의 프로젝트 이름도 사용할 수 있습니다. 소문자 `host/path`로 정규화하며, 스킴·인증정보·포트·쿼리·끝의 `/`와 `.git`을 제거합니다. SCP 형식인 `git@host:owner/repo`는 `host/owner/repo`가 됩니다. 따라서 `https://github.com/Org/Repo.git`과 `git@github.com:org/repo`는 모두 `project:github.com/org/repo`입니다. 생략하면 사용자 전체 메모리(`scope = user`)이며, 사용자 전체 메모리는 프로젝트별 조회에도 포함됩니다.
- **메모리 유형:** `semantic`, `episodic`, `project`, `preference`, `codebase`, `temporary`. [메모리 모델](../adr/0001-architecture.md#memory-model)(영문)을 참고하세요.
- **신뢰도:** `user`는 사용자가 확인한 메모리, `agent`는 에이전트가 작성한 메모리, `external`은 외부 콘텐츠에서 유래한 메모리입니다. 클라이언트는 `external`을 특히 주의해서 다뤄야 합니다.
- **상태:** `proposed`, `active`, `superseded`, `deleted`. 조회는 활성 상태이며 만료되지 않은 메모리만 반환합니다. 소유자가 중복·모순 정리나 세션 요약 제안을 적용해도 기존 메모리는 `superseded`가 됩니다. [정리 설계](../adr/0004-consolidation.md)(영문)를 참고하세요.
- **호출 에이전트:** `source_agent`는 도구 인자가 아니라 서버가 결정합니다. HTTP에서는 API 키의 에이전트 이름 또는 OAuth 승인 시 소유자가 지정한 이름입니다. stdio에서는 `KENFOLD_AGENT`를 사용하며, 없으면 클라이언트의 `clientInfo.name`을 소문자로 정리해 사용합니다. 아무 정보도 없으면 `unknown`입니다.
- **접근 권한:** API 키는 읽기·쓰기를 허용합니다. OAuth 권한은 `memory:read` 또는 읽기를 포함하는 `memory:write`입니다. 읽기 전용이어도 모든 도구가 목록에 표시되지만 `remember`, `handoff`, `resume`, `forget`은 권한 오류를 반환합니다. `resume`도 인계 수신자를 기록하므로 쓰기입니다. `get_context`, `recall`은 읽기 전용으로 표시됩니다. [원격 접속](deploy.md)을 참고하세요.
- **오류:** 잘못된 인자는 핸들러 실행 전에 스키마 검증으로 거절합니다. 잘못된 ID, 너무 긴 입력, 이미 교체된 메모리의 재교체 같은 도메인 오류는 설명이 포함된 도구 오류(`isError: true`)로 반환합니다. 내부 실패는 일반적인 오류만 반환하고 상세 내용은 서버 로그에 기록합니다.
- **비밀정보:** `remember`, `handoff`는 API 키·토큰·개인키·JWT·URL 속 비밀번호·비밀 필드에 지정된 무작위 형태의 값을 거절합니다. 오류에는 비밀정보의 종류만 표시하며 값은 반복하지 않습니다. `forget` 사유와 검색 쿼리는 거절 대신 비밀정보를 가립니다.
- **세션:** 호출의 `_meta`에 `{"kenfold/session_id": "<id>"}`를 넣으면 `source_session`으로 기록합니다. `kenfold hook`은 세션 요약에 이를 사용합니다.
- **로컬 경로:** `/home/me/repo`, `~/repo` 같은 경로는 기기마다 프로젝트가 달라지므로 `project` 값으로 허용하지 않습니다.
- **메모리는 데이터:** 반환된 `content`를 에이전트에 대한 지시로 해석해서는 안 됩니다.

## 검색

`recall`과 `get_context`의 `relevant` 영역은 다음 단계로 순위를 매깁니다. [검색 설계](../adr/0002-retrieval-and-code-refs.md)(영문)를 참고하세요.

1. **하이브리드 1차 검색**
   - 전문 검색은 쿼리 단어를 OR로 결합하고 `ts_rank`로 정렬합니다. PostgreSQL의 `simple` 설정을 사용해 어간 추출 없이 한국어·영어 혼합 텍스트를 처리합니다.
   - 임베딩 모델이 있으면 같은 모델로 생성한 벡터의 코사인 유사도를 계산합니다. `KENFOLD_SEARCH_MAX_DISTANCE`(기본 0.55)보다 먼 결과는 제외합니다.
   - 두 순위를 Reciprocal Rank Fusion(`k = 60`)으로 결합해 후보 30개를 구성합니다.
2. **연결 확장:** 재순위화 모델이 있으면 상위 8개 후보에서 한 단계 연결된 메모리를 추가합니다. 세션 요약에서 추출 사실로, 사실에서 원본 요약과 같은 세션의 다른 사실로 확장합니다.
3. **재순위화:** `KENFOLD_RERANK_URL`이 설정되어 있으면 교차 인코더가 상위 15개 후보와 쿼리를 함께 평가합니다. 실패하거나 4초를 넘기면 연결 확장 결과 없이 1차 순위를 유지합니다.
4. **추가 신호**
   - 참조 파일·심볼이 사라진 메모리는 점수의 5%, 참조 심볼이 바뀐 메모리는 60%를 유지합니다.
   - “last time”, “latest”, “지난번”, “최근”처럼 최근 작업을 묻는 쿼리는 모든 유형의 최신 메모리를 우선합니다. 반감기는 7일입니다.
   - 재순위화 모델이 있으면 세션 요약은 나이에 따라 점수가 최대 40% 감소하며 반감기는 30일입니다.

연결 확장과 일반적인 세션 요약의 시간 감쇠는 재순위화 점수에 맞춘 보정입니다. 내부 평가에서 1차 점수에 적용하면 순서를 지나치게 바꿨으므로 재순위화가 있을 때만 적용합니다.

`score`의 범위는 `(0, 1]`입니다. 재순위화가 있으면 추가 신호 적용 후의 관련성 확률이며, 없으면 결합 점수입니다. 결합 점수 1.0은 모든 순위 계산에서 1위였다는 뜻입니다.

임베딩 API가 실패하거나 10초를 초과해도 전문 검색으로 호출을 완료합니다. 그동안 임베딩 없이 저장된 메모리는 백그라운드에서 보충합니다.

## 코드 참조

프로젝트 범위의 `project`, `codebase`, `semantic` 메모리를 저장하거나 추출할 때 언급된 파일과 심볼을 기록합니다. 예를 들어 “`RequireAPIKey` in internal/auth/middleware.go hashes the token” 같은 내용입니다.

- 경로는 알려진 소스 확장자 또는 `Makefile`, `Dockerfile` 같은 파일명을 사용해야 합니다. URL, 절대 경로, `Next.js` 같은 제품명은 제외합니다.
- 심볼은 백틱이나 호출 형태(`Name()`)를 우선합니다. 파일 하나만 언급했을 때는 `CamelCase`, `snake_case` 같은 코드 형태의 식별자도 찾습니다.

서버는 저장소를 읽지 않습니다. 저장소 옆에서 실행하는 `kenfold hook`이 세션 시작 시 2초 이내로 검사하거나, `kenfold refs sync`가 직접 검사합니다. Git의 `post-commit` 훅에서도 실행할 수 있습니다.

클라이언트는 `HEAD`에서 파일의 blob ID 또는 tree-sitter로 찾은 심볼 정의의 공백 차이를 무시하는 해시를 기준값으로 기록합니다. 이후 같은 내용을 비교할 때 다음 규칙을 지킵니다.

- 참조 파일에 미커밋 변경이 있으면 기준값을 고정하지 않습니다.
- 현재 `HEAD`가 기준 커밋을 포함할 때만 변경을 보고합니다. 오래되었거나 갈라진 체크아웃은 변경의 증거로 사용하지 않습니다.

`code_refs[].state`는 `pending`(기준 없음), `current`, `changed`, `missing`(파일·심볼 없음)을 표시합니다. 파일·심볼이 사라지거나 심볼이 바뀌면 메모리가 `stale`이 됩니다. 파일 자체의 변경만으로는 오래된 메모리로 판정하지 않습니다.

오래된 메모리는 검색 순위가 낮아지고 `get_context`의 프로젝트 지식에서 뒤로 배치되며 세션 훅에도 표시됩니다. `kenfold refs status`와 `kenfold memory list --stale`로 검토할 수 있습니다.

## 도구

### `get_context` — 읽기 전용

작업 시작 시 토큰 예산 내에서 필요한 메모리를 불러옵니다.

| 인자 | 유형 | 필수 | 설명 |
|---|---|---|---|
| `task` | string | | 수행할 작업. `relevant` 검색에 사용 |
| `project` | string | | 공통 규칙 참고 |
| `budget_tokens` | integer | | 기본 2000, 200~32000 범위로 제한 |

다음 우선순위로 반환합니다.

1. `preferences[]`: 사용자 전체 및 프로젝트의 활성 선호
2. `handoff`: 아직 아무도 이어받지 않은 프로젝트의 최신 인계
3. `recent[]`: 훅이 작성한 최근 세션 요약 3개. 각각 최대 800자
4. `project[]`: 활성 프로젝트 메모리. 오래된 코드 관련 메모리는 뒤로 배치
5. `relevant[]`: `task`와 관련된 검색 결과

각 메모리는 한 번만 등장합니다. 예산에 맞지 않는 메모리는 제외하고 `truncated`를 설정합니다. 추가 조회에는 `recall`을 사용하세요. 토큰 수는 추정값입니다.

### `remember` — 멱등 저장

오래 유지할 메모리를 저장합니다.

| 인자 | 유형 | 필수 | 설명 |
|---|---|---|---|
| `content` | string | 예 | 독립적으로 이해할 수 있는 내용, 최대 8000자 |
| `type` | enum | | 생략 시 교체 대상의 유형 → 설정된 모델의 분류 → 프로젝트가 있으면 `project`, 없으면 `semantic` 순으로 결정 |
| `project` | string | | 생략하면 사용자 전체 범위 |
| `supersedes` | string (id) | | 같은 범위에서 교체할 활성 메모리 |
| `ttl_seconds` | integer | | `temporary` 유형에 필수, 최대 1년 |

`id`, `type`, `status`, `deduplicated`, `similar`를 반환합니다.

- **중복 제거:** 같은 유형·범위의 활성 또는 제안 메모리와 대소문자·공백·끝 문장부호만 다르면 `deduplicated: true`와 기존 메모리를 반환합니다. 같은 내용으로 교체하는 요청도 변경하지 않습니다.
- **유사 메모리:** 저장 후 같은 범위의 유사한 활성 메모리 최대 3개를 `similar[]`로 반환합니다. 임베딩 코사인 거리 ≤ 0.25 또는 trigram 유사도 ≥ 0.5가 기준입니다. 어느 내용이 맞는지는 자동 판정하지 않으며 에이전트가 교체·삭제합니다. 세션 요약과 임시 메모리는 검사하지 않습니다.
- **교체:** 대상은 같은 범위의 활성 메모리여야 합니다. 새 메모리가 활성이면 같은 트랜잭션에서 기존 메모리를 `superseded`로 바꾸고 이력으로 보존합니다. 새 메모리가 `proposed`이면 승인할 때까지 기존 메모리를 활성으로 유지합니다.
- **세션 요약:** 세션 ID와 함께 보낸 `episodic` 메모리는 같은 에이전트·세션의 이전 요약을 교체합니다.
- **검토:** 새 `preference`는 `proposed`입니다. `kenfold memory approve <id>`로 승인하면 제공을 시작하고 `trust = user`가 됩니다.

### `recall` — 읽기 전용

메모리를 자연어로 검색합니다.

| 인자 | 유형 | 필수 | 설명 |
|---|---|---|---|
| `query` | string | 예 | 최대 2000자로 잘라 사용 |
| `types` | enum[] | | 메모리 유형 필터 |
| `project` | string | | 사용자 전체 메모리는 항상 포함 |
| `limit` | integer | | 기본 10, 최대 50 |

`score` 순으로 정렬한 `memories[]`를 반환합니다. 로컬 CPU 재순위화를 사용하면 약 1~2초, 재순위화가 없는 하이브리드 검색은 약 25 ms입니다. 측정 조건은 [검색 품질](../../README.ko.md#검색-품질)을 참고하세요.

### `handoff`

다른 에이전트나 세션이 미완료 작업을 이어가도록 메모를 남깁니다. `temporary` 유형으로 저장합니다.

| 인자 | 유형 | 필수 | 설명 |
|---|---|---|---|
| `summary` | string | 예 | 수행한 작업과 현재 상태, 최대 8000자 |
| `next_steps` | string[] | | 최대 20개, 각각 최대 1000자 |
| `project` | string | | 프로젝트 |
| `ttl_seconds` | integer | | 기본 7일, 최대 30일 |

`id`, `expires_at`을 반환합니다. 프로젝트마다 활성 인계는 하나이며, 새 인계는 기존 인계를 교체하고 `supersedes`로 연결합니다.

### `resume` — 멱등 쓰기

| 인자 | 유형 | 필수 |
|---|---|---|
| `project` | string | |

프로젝트의 만료되지 않은 최신 인계를 `next_steps`와 함께 반환합니다. 인계가 없으면 `handoff` 필드가 없습니다. 첫 호출은 `resumed_by`와 수신 시간을 기록합니다. 이후 호출은 이 기록을 바꾸지 않고 같은 인계를 반환합니다.

이어받은 인계는 `get_context`에서 제외되지만, 만료되거나 교체될 때까지 `resume`으로 조회할 수 있습니다.

### `forget` — 파괴적 작업, 멱등

`status = deleted`로 논리 삭제합니다. 행은 감사·이력용으로 보존하며 더 이상 조회 결과로 제공하지 않습니다.

| 인자 | 유형 | 필수 | 설명 |
|---|---|---|---|
| `id` | string | 예 | 메모리 UUID |
| `reason` | string | | 최대 1000자로 잘라 사용 |

`id`, `status`를 반환합니다. 첫 삭제는 `forgotten_by`, `forget_reason`을 기록하며 다시 삭제해도 기록을 바꾸지 않습니다.

## 세션 훅

`kenfold hook`은 Claude Code·Codex의 명령형 훅이며 에이전트의 API 키로 인증하는 MCP 클라이언트입니다. 쓰기에는 다른 클라이언트와 같은 검사를 적용합니다.

| 이벤트 | 동작 |
|---|---|
| `SessionStart` | `cwd`의 Git 원격 URL로 프로젝트 식별. 이전 세션의 전송 대기 요약 전송. `get_context`를 예산 1500으로 호출해 참고 데이터로 표시한 `additionalContext` 반환. 실패 시 `systemMessage` 경고 후 세션 계속 |
| `UserPromptSubmit`, `Stop`, `PostCompact` | 요청·최종 응답·압축 요약을 로컬 로그에 추가. 비밀정보를 가리고 파일 권한은 0600. 네트워크 호출 없음 |
| `SessionEnd` | 요청·최종 응답·압축 요약에서 세션 요약을 구성하고 세션 ID를 포함한 `episodic` 메모리로 저장. 먼저 로컬 전송 대기 파일에 기록해 중단·서버 장애 시 다음 `SessionStart`에서 재전송 |

Git 저장소 밖의 세션은 기록하지 않으며 사용자 전체 컨텍스트만 조회합니다. 훅은 항상 종료 코드 0을 반환합니다. 설치는 [한국어 README](../../README.ko.md#세션-훅)를 참고하세요.

## 모델 기반 추출

`KENFOLD_CHAT_URL`을 설정하면 `kenfold serve`는 작성 후 수 분이 지난 세션 요약에서 오래 유지할 메모리를 추출합니다. 저장 전에 다음을 검사합니다.

- 프로젝트 규칙(`project`), 코드 사실(`codebase`), 사용자 선호(`preference`), 일반 사실(`semantic`)만 허용합니다. 작업·상태 보고·붙여 넣은 문서 내용은 제외합니다. 저장소 밖의 세션에서는 선호·일반 사실만 추출합니다.
- 모델이 제시한 근거 인용문이 원본 세션에 존재하고 긴 인용·코드 블록 밖에 있어야 합니다.
- 내용은 8~400자이며 비밀정보가 없어야 합니다.
- 같은 범위의 모든 상태에서 동일 내용이 없어야 합니다. 거절한 메모리도 중복 검사에 포함합니다.

작성자는 `kenfold-extractor`입니다. 원본의 `source_session`, `attrs.session_agent`, 근거인 `attrs.evidence`, 모델명인 `attrs.extractor_model`, 유사 메모리 ID인 `attrs.similar_to`와 원본 요약을 가리키는 `derived_from` 연결을 저장합니다.

기본 상태는 `proposed`이며 `kenfold memory review` 등으로 승인한 뒤 제공합니다. `KENFOLD_EXTRACT_POLICY=auto`에서는 명시적으로 서술되었고 기존 메모리와 유사하지 않은 결과를 활성화합니다. 사용자 선호와 `similar_to`가 있는 결과는 계속 검토 대상으로 남습니다.

같은 모델이 `remember`의 생략된 유형도 분류합니다. `attrs.type_source`는 `agent`, `model`, `superseded`(교체 대상의 유형 사용), `default`(모델 미설정·실패·15초 초과 등 기본 유형 사용)입니다.

## `MemoryView`

```json
{
  "id": "0199...",
  "type": "project",
  "scope": "project:github.com/kenfold/kenfold",
  "content": "HTTP 전송은 무상태 방식이므로 Mcp-Session-Id에 의존하지 않는다.",
  "source_agent": "claude-code",
  "trust": "agent",
  "created_at": "2026-09-27T06:00:00Z",
  "score": 0.83
}
```

선택 필드는 `expires_at`(임시 메모리·인계), `next_steps`·`resumed_by`(인계), `score`(검색 결과), `code_refs`(참조 파일·심볼 및 `state`, `checked_commit`), `stale`입니다.

## REST API

비에이전트 클라이언트는 `/api/v1`을 사용합니다. `/mcp`와 같은 Host 허용 목록, 교차 사이트 요청 차단, API 키 인증을 적용합니다. 오류 형식은 `{"error": "..."}`입니다.

`POST /api/v1/refs/check`에는 쓰기 권한이 필요합니다. 읽기 전용 OAuth 클라이언트에는 403을 반환합니다.

| 엔드포인트 | 용도 |
|---|---|
| `GET /api/v1/refs?project=P` | 프로젝트 `P`의 검사 대상. `{"scope", "targets": [{"path", "symbol", "anchor_commit", "anchor_hash"}]}` 형태. 기준값이 없는 항목부터 최대 500개 |
| `POST /api/v1/refs/check` | `{"project", "commit", "results": [{"path", "symbol", "anchor_commit", "found", "hash", "resolved_path"}]}` 보고. 결과 최대 1000개. 처리 후 상태별 개수 반환 |

`anchor_commit`이 없는 결과는 `commit`을 기준으로 참조를 고정합니다. `found: false`이면 `unresolved`로 표시하고 다음에 다시 제공합니다. `anchor_commit`이 있는 결과는 해당 커밋에서 고정한 해시와 비교합니다.

## OAuth 인증

`KENFOLD_PUBLIC_URL`이 설정되면 `/mcp`와 `/api/v1`은 OAuth 액세스 토큰도 받습니다. 유효한 토큰이 없으면 401과 다음 헤더를 반환합니다.

```text
WWW-Authenticate: Bearer resource_metadata="<public URL>/.well-known/oauth-protected-resource/mcp"
```

| 엔드포인트 | 용도 |
|---|---|
| `GET /.well-known/oauth-protected-resource/mcp` | 보호 리소스 메타데이터(RFC 9728). 리소스 `<public URL>/mcp`, 인증 서버, `memory:read`·`memory:write` 범위 |
| `GET /.well-known/oauth-authorization-server` | 인증 서버 메타데이터(RFC 8414). `/.well-known/openid-configuration`에서도 제공 |
| `GET, POST /oauth/authorize` | PKCE S256 인증 코드 흐름과 동의 페이지. 응답에 `iss` 포함(RFC 9207) |
| `POST /oauth/token` | `authorization_code`, `refresh_token` 교환. 클라이언트 인증은 `none` 또는 `private_key_jwt` |
| `POST /oauth/register` | 공개 클라이언트의 동적 등록(RFC 7591). `KENFOLD_OAUTH_DCR=false`로 비활성화 |
| `POST /oauth/revoke` | 토큰 철회(RFC 7009) |

- **클라이언트 식별:** HTTPS URL인 `client_id`는 클라이언트 메타데이터 문서를 가리킵니다. SSRF 방어를 적용해 가져옵니다.
- **리소스 지정:** `<public URL>/mcp`만 허용하며 토큰은 이 리소스에 묶입니다.
- **토큰 수명:** 액세스 토큰 1시간, 갱신 토큰 30일. 갱신 토큰은 사용할 때마다 교체합니다. 이미 교체된 갱신 토큰이나 사용한 인증 코드를 다시 제출하면 해당 클라이언트의 승인을 철회합니다.
