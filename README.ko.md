# Kenfold

[English](README.md) | [한국어](README.ko.md)

[![Release checks](https://github.com/jeonjw85/kenfold/actions/workflows/ci.yaml/badge.svg)](https://github.com/jeonjw85/kenfold/actions/workflows/ci.yaml)
[![Go 1.27+](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![PostgreSQL 18 + pgvector](https://img.shields.io/badge/PostgreSQL-18%20%2B%20pgvector-4169E1?logo=postgresql&logoColor=white)](compose.yaml)
[![MCP server](https://img.shields.io/badge/MCP-server-111111)](https://modelcontextprotocol.io)
[![MIT license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**모든 AI 에이전트가 함께 쓰는 하나의 메모리**

Kenfold는 Claude Code, Codex, OpenCode, ChatGPT, 로컬 모델이 [MCP](https://modelcontextprotocol.io)로 함께 사용하는 셀프 호스팅 메모리 서버

각 도구에 따로 저장되던 메모리를 모아 한 도구에서 내린 결정을 다른 도구에서도 활용

구성: 에이전트의 MCP 클라이언트, Kenfold 서버, PostgreSQL + pgvector 저장소

주요 설계 원칙은 아래와 같으며, 자세한 배경은 [ADR-0001](docs/adr/0001-architecture.md)(영문) 참고

- **유형별 메모리:** 일반 지식, 세션 기록, 프로젝트 규칙, 사용자 선호, 코드 정보, 임시 메모리에 서로 다른 수명과 검색 규칙 적용
- **출처 추적:** 작성 에이전트, 작성 시각, 신뢰도 기록, HTTP 작성자는 API 키 또는 승인된 OAuth 정보로 결정
- **이력 보존:** 새 메모리로 교체해도 기존 내용을 이력으로 보존
- **작업 인계:** Claude Code에서 멈춘 작업을 Codex에서 계속 진행
- **메모리 오염 방어:** 메모리는 참고 데이터로 전달, 사용자 선호는 승인 후 사용

**현재 상태: 5단계 기능 구현 완료, 공개 벤치마크 검증 미완료**

- 구현 완료: 공유 저장소, 하이브리드 검색, 세션 훅, 메모리 추출, 코드 참조, OAuth 원격 접속, 내보내기와 가져오기, 검토 대시보드, 메모리 정리
- 최근 측정: raw-LoCoMo 1,540문항 중 1,402문항의 부분 결과 보존, API 사용량 확인 불가로 중단
- 추출 메모리 기반 LoCoMo와 LongMemEval QA는 미채점
- [부분 결과와 측정 한계](internal/bench/testdata/RESULTS.md)(영문) 참고

## 빠른 시작

- 필요 도구: Git, Make, Docker와 Compose
- 개발하거나 호스트에서 CLI와 훅을 빌드할 때 Go 1.27 이상 필요
- 저장소를 내려받은 디렉터리에서 실행

```sh
make up                       # PostgreSQL 18 + pgvector와 Kenfold 실행
make key AGENT=claude-code     # Claude Code용 API 키 발급: 한 번만 표시됨
make key AGENT=codex           # 작성자를 구분할 수 있도록 에이전트마다 별도 키 사용
```

기본 주소는 `http://127.0.0.1:7077`, `make up`은 전문 검색 제공

표현이 다른 메모리까지 찾으려면 로컬 검색 모델을 함께 실행

```sh
make up-embed                 # Ollama + bge-m3 + llama.cpp 재순위화 모델
```

- 첫 실행 시 약 1.9 GB 다운로드
- 임베딩 없이 저장한 메모리는 모델 준비 후 자동 임베딩
- 재순위화 모델은 쿼리와 후보 메모리를 함께 읽고 순서 조정
- 내부 평가 recall@5는 0.90에서 0.96으로 상승, CPU 검색당 1~2초 추가
- `make up-embed RERANK=0`으로 재순위화 비활성화

## 업데이트

```sh
git pull && make up
```

로컬 검색이나 추출 모델을 사용한다면 기존 실행 방식에 맞춰 `make up-embed` 또는 `make up-extract` 사용

릴리스 빌드는 GitHub의 새 버전을 대시보드 하단과 `kenfold version`에 표시

자동 다운로드나 재시작은 없으며, `KENFOLD_UPDATE_CHECK=false`로 확인 기능 비활성화

## 에이전트 연결

에이전트마다 별도 API 키 발급, 키에 연결된 이름을 메모리 작성자로 기록

### Claude Code

```sh
claude mcp add --scope user --transport http kenfold http://127.0.0.1:7077/mcp \
  --header "Authorization: Bearer kf_..."
```

`kf_...`는 발급받은 키로 교체

### Codex

Codex 실행 환경에 `KENFOLD_CODEX_KEY`를 설정하고 `~/.codex/config.toml`에 아래 내용 추가

```toml
[mcp_servers.kenfold]
url = "http://127.0.0.1:7077/mcp"
bearer_token_env_var = "KENFOLD_CODEX_KEY"
```

셸 환경 변수를 읽지 못하는 데스크톱 앱이나 IDE 확장에서는 `http_headers = { "Authorization" = "Bearer kf_..." }` 사용 가능

### OpenCode

프로젝트의 `opencode.json` 또는 `~/.config/opencode/opencode.json`에 아래 내용을 추가하고, 실행 환경에 `KENFOLD_OPENCODE_KEY` 설정

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "kenfold": {
      "type": "remote",
      "url": "http://127.0.0.1:7077/mcp",
      "oauth": false,
      "headers": { "Authorization": "Bearer {env:KENFOLD_OPENCODE_KEY}" }
    }
  }
}
```

### stdio 연결

클라이언트가 Kenfold를 하위 프로세스로 실행하는 방식

- `make build` 후 `bin/kenfold mcp` 실행
- `KENFOLD_AGENT`에 에이전트 이름 지정
- 데이터베이스에 직접 연결하므로 API 키 불필요
- `KENFOLD_DATABASE_URL`의 기본값은 Compose 데이터베이스

```sh
claude mcp add --env KENFOLD_AGENT=claude-code --transport stdio --scope user kenfold -- /abs/path/to/bin/kenfold mcp
```

```toml
[mcp_servers.kenfold]
command = "/abs/path/to/bin/kenfold"
args = ["mcp"]
env = { KENFOLD_AGENT = "codex" }
```

`make up-embed` 사용 시 stdio 프로세스에도 `KENFOLD_EMBED_URL=http://127.0.0.1:11435/v1` 설정

### ChatGPT, claude.ai, 다른 기기

터널이나 리버스 프록시를 통한 공개 HTTPS 주소 필요

```sh
KENFOLD_PUBLIC_URL=https://kenfold.example.com make up
docker compose exec kenfold /usr/local/bin/kenfold password
```

- 내장 OAuth 2.1 서버 활성화
- 소유자 비밀번호로 각 클라이언트를 승인하고 읽기 전용 또는 읽기와 쓰기 권한 선택
- 같은 비밀번호로 대시보드 로그인
- Tailscale Funnel, Cloudflare Tunnel, Caddy 구성은 [원격 배포 안내](docs/ko/deploy.md) 참고

## 세션 훅

MCP 도구는 모델이 호출을 결정할 때만 실행

Claude Code와 Codex의 훅을 설정하면 메모리 자동 활용 가능

- **세션 시작:** 현재 저장소의 인계 메모, 최근 세션, 프로젝트 지식, 사용자 선호 조회
- **세션 중:** 사용자 요청, 최종 응답, Claude Code의 컨텍스트 압축 요약을 `~/.local/state/kenfold`에 기록, 소유자만 읽기 가능, 비밀정보 가림, 네트워크 호출 없음
- **세션 종료:** 로그를 짧은 `episodic` 메모리로 저장, 서버 연결 실패 시 다음 세션 시작 때 재전송

Git 저장소 밖의 세션은 기록하지 않음

훅 오류는 경고로 알리고 에이전트 작업은 계속 진행

대기 중인 요약은 원래 서버 URL, 클라이언트 이름, 인증정보에 묶어 다른 에이전트나 서버로 전송하지 않음

소유자를 확인할 수 없는 구형 대기 파일은 경고와 함께 보관, 대기 파일은 30일 후 만료

```sh
make build
mkdir -p ~/.config/kenfold && (umask 077; make -s key AGENT=claude-code > ~/.config/kenfold/claude-code.key)
bin/kenfold hook config claude-code --key-file ~/.config/kenfold/claude-code.key
```

- Claude Code: 출력된 `hooks` 객체를 `~/.claude/settings.json`에 병합
- Codex: `codex`용 키와 `bin/kenfold hook config codex`를 사용하고 결과를 `~/.codex/hooks.json`에 저장
- Codex에서 `/hooks`를 한 번 실행해 새 훅 검토 후 승인

`--no-capture`는 세션 기록 없이 시작 시 메모리만 조회, `--no-refs`는 시작 시 코드 참조 검사 비활성화

## 메모리 추출

채팅 모델 설정 시 세션 요약에서 오래 유지할 프로젝트 규칙, 코드 사실, 사용자 선호를 추출해 제안

일회성 작업과 붙여 넣은 문서의 내용은 제외

```sh
make up-extract                # 검색 모델 + qwen3.5:4b, 최초 총 다운로드 약 5.2 GB
make review                    # 제안된 메모리를 승인, 거절하거나 기존 메모리와 교체
```

- 추출 결과는 `proposed` 상태로 저장, CLI 또는 [대시보드](#대시보드)에서 승인한 뒤 에이전트에 제공
- 각 제안에 출처 인용문과 유사한 기존 메모리 표시
- 교체 승인 시 기존 메모리를 이력으로 보존
- 거절한 내용은 다시 제안하지 않음

- 메모리 유형 생략 시 같은 모델로 분류
- CPU에서 `remember` 호출에 약 6초 추가, 15초 초과 시 기본 유형 사용
- 백그라운드 세션 추출은 CPU에서 약 10~30초 소요

- 성능 코어와 효율 코어가 나뉜 CPU에서는 `THREADS`를 성능 코어 수로 설정
- 기본값은 4, 변경 예시는 `make up-extract THREADS=6`
- `KENFOLD_CHAT_URL`과 `KENFOLD_CHAT_MODEL`로 OpenAI 호환 채팅 API 사용 가능
- [내부 추출 평가](internal/extract/testdata/RESULTS.md)(영문)는 `make eval`로 실행

`KENFOLD_EXTRACT_POLICY=auto`는 명시적으로 서술되었고 기존 메모리와 유사하지 않은 추출 결과를 자동 활성화

사용자 선호는 항상 검토

붙여 넣은 내용의 지시가 메모리에 섞일 수 있어 기본값인 `propose`는 모든 추출 결과를 검토

## 메모리 정리

채팅 모델 설정 시 중복되거나 모순되는 메모리 정리와 오래된 세션 요약 제안

- **중복:** 한 메모리가 다른 메모리의 내용을 모두 포함하면 더 많은 정보를 가진 쪽 유지
- **모순:** 서로 충돌하는 내용은 더 최근 메모리를 남기는 안 제안
- **오래된 세션:** 한 프로젝트에 30일이 지난 세션 요약이 6개 이상이면 가장 오래된 최대 10개를 하나로 요약

소유자가 적용한 뒤에만 변경하고 기존 메모리는 이력으로 보존

중복이나 모순을 정리할 때는 내용을 새로 작성하지 않고 기존 메모리 하나를 그대로 유지

```sh
bin/kenfold consolidate list
bin/kenfold consolidate apply <id>    # 또는 reject <id>
bin/kenfold consolidate run           # 정기 실행을 기다리지 않고 지금 검사
```

- 후보 쌍의 순서를 바꿔 두 번 판단하고 결과가 일치할 때만 제안
- 세션을 묶은 요약의 파일명, 식별자, 숫자는 원문에 있는 값만 허용
- 작업자는 15분마다 실행, 추출 작업 우선
- `KENFOLD_CONSOLIDATE=false`로 비활성화
- [설계](docs/adr/0004-consolidation.md)와 [내부 평가](internal/consolidate/testdata/RESULTS.md)는 영문 참고

## 코드 참조

코드 변경에 따라 오래된 메모리를 구분하기 위해 언급된 파일과 심볼 기록

세션 시작 시 훅이 저장소 `HEAD`에서 검사하고, 심볼 변경이나 파일 또는 심볼 삭제 시 `stale`로 표시해 검색 순위 하향

- 커밋된 코드만 검사
- 파일에 미커밋 변경이 있으면 기준 해시 고정 보류
- 기준 커밋을 포함하지 않는 오래된 체크아웃이나 다른 브랜치에서는 변경을 보고하지 않음
- 파일 자체의 변경만으로는 오래된 메모리로 판정하지 않음

```sh
bin/kenfold refs sync                 # KENFOLD_API_KEY 또는 --key-file 사용
bin/kenfold refs status
bin/kenfold memory list --stale
```

커밋마다 검사하려면 `.git/hooks/post-commit` 등에 아래 내용 추가

```sh
#!/bin/sh
/abs/path/to/bin/kenfold refs sync --quiet --key-file ~/.config/kenfold/claude-code.key || true
```

`make build`는 Go, TypeScript/TSX, JavaScript, Python, Rust, Java, Kotlin, Ruby, C#, PHP, C, C++, Swift, Scala, Bash의 심볼 탐색 지원

`GRAMMARS=...`로 포함할 언어 변경 가능, 나머지 파일은 파일 단위로 추적

## 메모리 동작 방식

- **프로젝트 식별:** Git 원격 URL 정규화, `git@github.com:Org/Repo.git`과 `https://github.com/org/repo`는 같은 프로젝트로 처리, 로컬 경로는 거절, 프로젝트 생략 시 사용자 전체 범위로 모든 프로젝트에서 조회
- **저장과 교체:** `remember`로 독립적으로 이해할 수 있는 내용 저장, 대소문자와 공백 및 끝 문장부호만 다른 중복은 기존 메모리 반환, `supersedes`로 교체 시 기존 내용은 이력으로 보존, 유사 메모리는 `similar`로 반환하며 자동 병합 없음
- **비밀정보 차단:** 일반적인 API 키, 토큰, 개인키, URL이나 대입문 속 비밀번호, JWT가 포함된 쓰기 거절, `bin/kenfold scan`으로 기존 메모리 검사, `--redact`로 비밀정보 가림
- **사용자 선호:** 에이전트가 작성한 `preference`는 승인 후 제공하고 `trust = user` 적용
- **인계:** `handoff`는 프로젝트마다 활성 메모 하나 유지, `resume`은 이를 반환하고 수신자 기록, `get_context`는 아직 이어받지 않은 인계만 제공
- **검색:** `recall`은 전문 검색과 벡터 검색 순위를 결합하고 연결된 메모리 확장, 모델 설정 시 재순위화 수행, 최근 작업 질문에는 최신 메모리 우선, 오래된 코드 관련 메모리는 순위 하향, 모델 장애 시 사용 가능한 단계로 검색 계속

```sh
make build
bin/kenfold memory review
bin/kenfold memory list --status proposed
bin/kenfold memory approve <id>
bin/kenfold memory forget <id> --reason "오래된 정보"
```

## 대시보드

`http://127.0.0.1:7077/dashboard/`에서 소유자 비밀번호로 로그인

최초 사용 시 아래 명령으로 비밀번호 설정

```sh
docker compose exec kenfold /usr/local/bin/kenfold password
```

- **Review:** 추출 결과와 사용자 선호를 오래된 순서대로 검토한 뒤 승인, 거절, 교체
- **Consolidation:** 중복이나 모순 정리, 오래된 세션 요약 제안을 적용하거나 거절
- **Memories:** 메모리 검색과 범위별, 유형별, 상태별 조회, 출처와 이력 및 코드 참조 확인, 삭제 처리
- **Clients:** API 키와 OAuth 클라이언트의 최근 사용 확인 및 철회, 새 키는 서버에서만 발급

- 기본 대시보드는 루프백 호스트 이름에서만 응답, 프록시나 터널을 거친 요청 거절
- SSH 포트 포워딩과 `KENFOLD_DASHBOARD=remote` 설정은 [배포 문서](docs/ko/deploy.md#대시보드-로컬-접근) 참고
- JavaScript 없이 동작, 변경 요청에는 세션의 CSRF 토큰 필요
- 세션 종료 조건: 2시간 미사용, 최대 12시간 경과, 비밀번호 변경, 서버 재시작

## 백업과 서버 이전

```sh
docker compose exec -T kenfold /usr/local/bin/kenfold export > kenfold-backup.jsonl
bin/kenfold export --out backup.jsonl --scope github.com/org/repo --active
bin/kenfold import --dry-run backup.jsonl && bin/kenfold import backup.jsonl
```

- 포함 항목: 메모리, 이력, 출처, 타임스탬프, 연결, 코드 참조
- 제외 항목: 임베딩, API 키, OAuth 클라이언트
- 대상 서버의 모델로 다시 임베딩
- 기존 메모리는 유지하므로 가져오기를 반복해도 중복 생성 없음
- 불완전한 아카이브나 비밀정보가 포함된 메모리가 있으면 전체 가져오기 거절

- `--out`으로 만든 파일은 소유자만 읽기 가능
- 아카이브는 메모리를 평문으로 담으므로 데이터베이스와 같은 수준으로 보관
- 셸 리다이렉션으로 만드는 파일의 권한은 셸의 `umask` 적용

## 검색 품질

3개 프로젝트의 메모리 110개, 한국어와 영어 쿼리 67개로 구성된 내부 평가

`make eval-search`로 실행, Recall@5는 질문에 답하는 메모리 중 상위 5개 결과에서 찾은 비율

| 구성 | 개발 세트 43개 | 홀드아웃 24개 | 검색 시간 |
|---|---|---|---|
| `make up` (전문 검색) | 0.65 | 0.44 | 수 ms |
| 2단계 하이브리드 검색 | 0.85 | 0.88 | 약 25 ms |
| `make up-embed RERANK=0` | 0.90 | 0.90 | 약 25 ms |
| `make up-embed` (재순위화 포함) | **0.96** | **0.96** | 약 1.8초 |

- Apple M5 CPU에서 모델을 미리 로드한 상태로 측정
- 개발 세트는 파이프라인 조정에 사용
- 홀드아웃은 순위 계산 코드를 보지 않은 별도 작성자가 이후에 작성
- 작은 합성 데이터셋의 동작 확인용 평가로, 다른 시스템과의 성능 비교에는 사용 불가
- [상세 결과](internal/retrieve/testdata/RESULTS.md)(영문) 참고

## MCP 도구

| 도구 | 용도 |
|---|---|
| `get_context` | 작업 시작 시 선호, 인계, 최근 세션, 프로젝트 지식, 관련 메모리 조회 |
| `remember` | 오래 유지할 메모리 저장 및 기존 메모리 교체 |
| `recall` | 자연어 검색 |
| `handoff` | 다음 에이전트나 세션에 작업 인계 |
| `resume` | 최신 인계 메모를 이어받음 |
| `forget` | 메모리 논리 삭제 |

인자와 반환값은 [MCP 도구 명세](docs/ko/mcp-tools.md) 참고

## 보안

- 포트는 기본적으로 `127.0.0.1`에만 공개, 원격 접속은 TLS 터널 또는 리버스 프록시와 `KENFOLD_PUBLIC_URL` 사용
- `/mcp`는 기본적으로 API 키 또는 OAuth 액세스 토큰 필요, SHA-256 해시만 저장, `bin/kenfold key revoke <prefix>` 또는 `bin/kenfold oauth revoke <grant>`로 철회
- 단일 소유자용 서버로 모든 유효한 키와 승인된 OAuth 클라이언트는 전체 메모리 읽기 가능, 프로젝트 범위는 접근 격리를 제공하지 않음
- OAuth 쓰기 권한은 별도 승인, 기본 선택은 읽기 전용
- 소유자 비밀번호는 Argon2id로 저장, 동의 페이지와 대시보드에서 합산 5회 실패 시 양쪽 로그인 15분 잠금
- `/mcp`와 `/api/v1`은 Host 허용 목록과 교차 사이트 요청 차단 적용, `/healthz`와 `/readyz`는 인증 없이 상태만 제공
- `KENFOLD_AUTH=none`은 인증 비활성화, 작성자는 클라이언트가 전달한 이름으로 기록
- 비밀정보 필터는 알려진 형식과 비밀 필드의 무작위 형태 값을 찾는 패턴 기반 검사로 모든 비밀정보 탐지는 불가, 에이전트에도 비밀정보 저장 금지 지시 필요

## CLI

```text
kenfold serve                        HTTP 서버 실행
kenfold healthcheck                  서버와 DB 스키마 준비 상태 확인
kenfold mcp                          stdio MCP 서버 실행
kenfold migrate [up|down|status]      DB 마이그레이션
kenfold key create <agent>           API 키 발급: stdout에 한 번만 표시
kenfold key list [--all]             키 목록
kenfold key revoke <id|prefix>       키 철회
kenfold memory list [--status S] [--type T] [--scope S] [--stale] [--limit N]
kenfold memory review [--scope S]
kenfold memory approve <id>... [--replaces ID]
kenfold memory reject <id>... [--reason R]
kenfold memory forget <id> [--reason R]
kenfold extract status
kenfold extract run [--limit N]
kenfold consolidate status|list
kenfold consolidate run [--limit N]
kenfold consolidate apply|reject <id>...
kenfold password                     소유자 비밀번호 설정
kenfold oauth clients [--all]
kenfold oauth revoke <grant>
kenfold refs sync [--dir D] [--quiet]
kenfold refs status [--scope S]
kenfold export [--out F] [--scope S] [--active]
kenfold import [--dry-run] <F|->
kenfold reindex                      현재 모델의 임베딩이 없는 메모리 처리
kenfold scan [--redact]              저장된 비밀정보 탐지와 가림
kenfold hook [--no-capture] [--no-refs]
kenfold hook config <claude-code|codex> [--key-file F]
kenfold version
```

## 설정

| 환경 변수 | 기본값과 설명 |
|---|---|
| `KENFOLD_HTTP_ADDR` | `127.0.0.1:7077` |
| `KENFOLD_DATABASE_URL` | `postgres://kenfold:kenfold@127.0.0.1:54329/kenfold?sslmode=disable` (로컬 Compose DB) |
| `KENFOLD_DATABASE_PASSWORD` | 미설정, PostgreSQL URL의 비밀번호를 별도로 지정하고 자동 이스케이프, Compose는 `KENFOLD_DB_PASSWORD` 전달 |
| `KENFOLD_AUTO_MIGRATE` | `false`, Compose는 `true`, `serve`와 `mcp`는 오래된 스키마에서 실행 거절 |
| `KENFOLD_ALLOWED_HOSTS` | `localhost,127.0.0.1,::1` (Host 허용 목록) |
| `KENFOLD_AUTH` | `apikey` 또는 `none` |
| `KENFOLD_PUBLIC_URL` | 미설정, 경로 없는 공개 HTTPS URL, 호스트를 허용 목록에 추가하고 OAuth 활성화 |
| `KENFOLD_OAUTH` | 공개 URL 설정 시 활성화 |
| `KENFOLD_OAUTH_DCR` | `true`, 동적 클라이언트 등록 허용 |
| `KENFOLD_DASHBOARD` | `local`은 로컬 전용, `remote`는 공개 URL에서도 제공, `off`는 비활성화 |
| `KENFOLD_AGENT` | stdio 서버의 에이전트 이름 |
| `KENFOLD_EMBED_URL` | 미설정 시 전문 검색만 사용, OpenAI 호환 임베딩 API 주소 |
| `KENFOLD_EMBED_MODEL` | `bge-m3`, 반드시 1024차원 벡터 출력 |
| `KENFOLD_EMBED_NAME` | 미설정, 모델 별칭과 별도로 벡터에 기록할 이름, `make up-embed`는 `kenfold-embed`를 실행하고 `bge-m3`로 기록 |
| `KENFOLD_EMBED_API_KEY` | 미설정, 임베딩 API에 전달할 Bearer 토큰 |
| `KENFOLD_EMBED_DIMENSIONS` | `false`, `true`이면 `dimensions=1024` 전달 |
| `KENFOLD_SEARCH_MAX_DISTANCE` | `0.55`, 벡터 검색의 최대 코사인 거리 |
| `KENFOLD_RERANK_URL` | 미설정 시 재순위화 비활성화, llama.cpp, Jina, Cohere, Voyage 등의 `/rerank` API |
| `KENFOLD_RERANK_MODEL` | `bge-reranker-v2-m3` |
| `KENFOLD_RERANK_API_KEY` | 미설정, 재순위화 API에 전달할 Bearer 토큰 |
| `KENFOLD_CHAT_URL` | 미설정 시 추출 비활성화, OpenAI 호환 채팅 API 주소 |
| `KENFOLD_CHAT_MODEL` | `qwen3.5:4b`, `make up-extract`는 스레드 수를 제한한 별칭 `kenfold-extract` 사용 |
| `KENFOLD_CHAT_API_KEY` | 미설정, 채팅 API에 전달할 Bearer 토큰 |
| `KENFOLD_CHAT_REASONING` | `none`을 `reasoning_effort`로 전달, 이 필드를 거절하는 서버에는 `omit` 사용 |
| `KENFOLD_EXTRACT` | 채팅 모델 설정 시 활성화 |
| `KENFOLD_EXTRACT_POLICY` | `propose`는 모두 검토, `auto`는 조건을 충족한 추출 결과 자동 활성화 |
| `KENFOLD_CLASSIFY` | 채팅 모델 설정 시 활성화, 유형이 생략된 메모리 분류 |
| `KENFOLD_CONSOLIDATE` | 채팅 모델 설정 시 활성화, 중복이나 모순 정리와 오래된 세션 요약 제안 |
| `KENFOLD_LOG_LEVEL` | `info` |
| `KENFOLD_UPDATE_CHECK` | `true`, 릴리스 빌드의 새 버전 확인, 자동 설치나 재시작 없음 |

- 훅과 `kenfold refs sync`는 `KENFOLD_URL`(기본 `http://127.0.0.1:7077/mcp`)과 `KENFOLD_API_KEY` 사용
- `--key-file` 지정 시 해당 파일의 키 사용
- 훅의 상태 디렉터리는 `KENFOLD_STATE_DIR`(기본 `~/.local/state/kenfold`)로 변경

임베딩 모델 변경

- 다른 1024차원 모델로 변경할 때는 마이그레이션 불필요
- 현재 모델로 생성한 벡터끼리만 비교하고 나머지는 백그라운드에서 다시 임베딩, `kenfold reindex`로 직접 실행 가능
- 다른 차원으로 변경하려면 마이그레이션 필요

## 개발

```sh
make test               # 단위 테스트 + stdio E2E, DB 불필요
make check              # gofmt, vet, race 테스트, 빌드, 취약점 검사
make test-integration   # Compose PostgreSQL의 별도 테스트 DB 사용
make lint               # gofmt + go vet
make eval               # 추출과 정리 품질 평가, up-extract 필요
make eval-search        # 검색 단계별 평가, up-embed 필요
make bench              # LoCoMo + LongMemEval_S 일부 평가
make build              # bin/kenfold 빌드
make logs               # 서버 로그
make down               # 스택 종료, 데이터 볼륨 유지
```

`make check`에 통합 테스트를 포함하려면 `KENFOLD_TEST_DATABASE_URL`을 폐기 가능한 PostgreSQL 18 + pgvector 데이터베이스로 설정

- 테스트는 데이터를 비우고 패키지를 순차 실행
- GitHub Actions는 별도 DB로 검사하고 프로덕션 컨테이너 빌드

- 공개 벤치마크는 외부 API 비용 발생 가능
- `KENFOLD_BENCH_CHAT_URL`, `KENFOLD_BENCH_API_KEY`, 예산, 입출력 단가 설정 필요
- `KENFOLD_BENCH_REASONING=none` 사용, 단가는 읽기 모델과 채점 모델 모두의 컨텍스트 구간별 요금 포함
- 사용량 누락 시 호출 중단, 미완료 결과는 `PARTIAL`로 보존

- 평가 재시작을 위해 문항별 점수 캐시와 사용량 저널 함께 보존
- 데이터, 코드, 모델 가중치와 설정이 일치하는 실행 식별자 필요
- 불확실한 진행 중 호출은 자동 재시도하지 않음
- 예산은 기본적으로 실행별 적용
- 상세 변수, 복구 절차, 추출 체크포인트는 [영문 개발 안내](README.md#development) 참고
- [복구 검사](internal/bench/testdata/RESULTS.md#free-recovery-verification--no-quality-scores)는 운영 검증으로, QA 품질 점수에 해당하지 않음

### 디렉터리 구조

```text
cmd/kenfold/          CLI, 서버 실행, 훅, 관리 명령, E2E 테스트
internal/mcpserver/  MCP 도구 스키마와 핸들러
internal/store/      PostgreSQL 저장, 하이브리드 검색, 메모리 연결
internal/retrieve/   검색, 확장, 재순위화 파이프라인과 평가 데이터
internal/coderef/    코드 참조 추출, Git과 tree-sitter 검사, 동기화
internal/restapi/    /api/v1 REST API
internal/dashboard/ 소유자용 서버 렌더링 대시보드
internal/hook/      Claude Code와 Codex 세션 훅
internal/extract/   메모리 추출과 유형 분류
internal/consolidate/ 메모리 정리 제안과 평가
internal/bench/     LoCoMo와 LongMemEval 평가 도구
internal/secrets/   인증정보 탐지와 가림
internal/oauth/     OAuth 2.1 서버
internal/config/    환경 변수 설정
migrations/         바이너리에 포함되는 SQL 마이그레이션
deploy/             원격 접속용 리버스 프록시 예제
docs/               배포 안내, 도구 명세, 설계 결정 기록
```

## 로드맵

| 단계 | 상태 | 범위 |
|---|---|---|
| 0 | 완료 | 스키마, MCP 계약, 프로젝트 골격, Compose |
| 1 | 완료 | 저장과 하이브리드 검색, API 키, CLI, 에이전트 연결 |
| 2 | 완료 | 비밀정보 필터, 중복과 모순 힌트, 자동 세션 요약 훅 |
| 2b | 완료 | 검토 기반 메모리 추출, 유형 분류 |
| 3 | 완료 | 재순위화, 메모리 연결 확장, 최신성과 코드 변경 반영, REST API |
| 4 | 완료 | OAuth 2.1과 원격 배포, SDK 클라이언트로 HTTPS E2E 검증, 실제 ChatGPT 연결은 미검증, 객체 저장소와 OpenAI 호환 프록시는 [보류](docs/adr/0003-remote-access-and-oauth.md) |
| 5 | 일부 완료 | 내보내기와 가져오기, 대시보드, 메모리 정리 완료, 공개 벤치마크 검증 미완료 |
