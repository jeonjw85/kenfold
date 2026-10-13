# Kenfold

[English](README.md) | [한국어](README.ko.md)

[![Release checks](https://github.com/jeonjw85/kenfold/actions/workflows/ci.yaml/badge.svg)](https://github.com/jeonjw85/kenfold/actions/workflows/ci.yaml)
[![Go 1.27+](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![PostgreSQL 18 + pgvector](https://img.shields.io/badge/PostgreSQL-18%20%2B%20pgvector-4169E1?logo=postgresql&logoColor=white)](compose.yaml)
[![MCP server](https://img.shields.io/badge/MCP-server-111111)](https://modelcontextprotocol.io)
[![MIT license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**모든 AI 에이전트가 함께 쓰는 하나의 메모리.**

Claude Code, Codex, OpenCode, ChatGPT, 로컬 모델은 각자 별도의 메모리를 사용합니다. Kenfold는 이 도구들이 [MCP](https://modelcontextprotocol.io)를 통해 함께 사용하는 셀프 호스팅 메모리 서버입니다. 한 도구에서 내린 결정을 다른 도구에서도 이어서 활용할 수 있습니다.

```text
Claude Code ─┐
Codex ───────┤
OpenCode ────┼── Kenfold ── PostgreSQL + pgvector
ChatGPT ─────┤
Local LLM ───┘
```

주요 설계 원칙은 다음과 같습니다. 자세한 배경은 [ADR-0001](docs/adr/0001-architecture.md)(영문)을 참고하세요.

- **유형별 메모리:** 일반 지식, 세션 기록, 프로젝트 규칙, 사용자 선호, 코드 정보, 임시 메모리에 각각 다른 수명과 검색 규칙을 적용합니다.
- **출처 추적:** 어떤 에이전트가 언제 작성했는지, 얼마나 신뢰할 수 있는지 기록합니다. HTTP 요청의 작성자는 클라이언트가 주장하는 이름이 아니라 API 키 또는 승인된 OAuth 정보로 결정합니다.
- **이력 보존:** 새 메모리로 교체할 때 기존 내용을 덮어쓰지 않고 이력으로 남깁니다.
- **작업 인계:** Claude Code에서 멈춘 작업을 Codex에서 이어갈 수 있습니다.
- **메모리 오염 방어:** 메모리는 지시가 아닌 참고 데이터로 전달하며, 사용자 선호는 승인 후에 사용합니다.

> **현재 상태: 5단계 기능 구현 완료, 공개 벤치마크 검증은 미완료입니다.** 공유 저장소, 하이브리드 검색, 세션 훅, 메모리 추출, 코드 참조, OAuth 원격 접속, 내보내기·가져오기, 검토 대시보드, 메모리 정리를 구현했습니다. 최근 측정에서는 raw-LoCoMo 1,540문항 중 1,402문항의 부분 결과를 보존했지만 API 사용량을 확인할 수 없어 중단했습니다. 추출 메모리 기반 LoCoMo와 LongMemEval QA는 채점하지 않았습니다. [부분 결과와 측정 한계](internal/bench/testdata/RESULTS.md)(영문)를 참고하세요.

## 빠른 시작

Git, Make, Docker와 Compose가 필요합니다. Go 1.27 이상은 개발하거나 호스트에서 CLI·훅을 빌드할 때 필요합니다. 저장소를 내려받은 디렉터리에서 실행하세요.

```sh
make up                       # PostgreSQL 18 + pgvector와 Kenfold 실행
make key AGENT=claude-code     # Claude Code용 API 키 발급: 한 번만 표시됨
make key AGENT=codex           # 작성자를 구분할 수 있도록 에이전트마다 별도 키 사용
```

기본 주소는 `http://127.0.0.1:7077`입니다. `make up`은 전문 검색을 제공합니다. 표현이 다른 메모리까지 찾으려면 로컬 검색 모델을 함께 실행하세요.

```sh
make up-embed                 # Ollama + bge-m3 + llama.cpp 재순위화 모델
```

첫 실행 시 약 1.9 GB를 다운로드합니다. 임베딩 없이 저장한 메모리는 모델이 사용 가능해지면 자동으로 임베딩합니다. 재순위화 모델은 쿼리와 후보 메모리를 함께 읽고 순서를 조정합니다. 내부 평가에서는 recall@5가 0.90에서 0.96으로 높아졌으며, CPU에서 검색당 1~2초가 추가됩니다. `make up-embed RERANK=0`으로 재순위화를 끌 수 있습니다.

## 업데이트

```sh
git pull && make up
```

로컬 검색·추출 모델을 사용한다면 기존 실행 방식에 맞춰 `make up-embed` 또는 `make up-extract`를 사용하세요.

릴리스 빌드는 GitHub에서 새 버전을 확인해 대시보드 하단과 `kenfold version`에 표시합니다. 자동 다운로드나 재시작은 하지 않습니다. `KENFOLD_UPDATE_CHECK=false`로 확인 기능을 끌 수 있습니다.

## 에이전트 연결

각 에이전트에 별도 API 키를 발급하세요. 키에 연결된 이름이 메모리 작성자로 기록됩니다.

### Claude Code

```sh
claude mcp add --scope user --transport http kenfold http://127.0.0.1:7077/mcp \
  --header "Authorization: Bearer kf_..."
```

`kf_...`는 발급받은 키로 바꾸세요.

### Codex

Codex를 시작하는 환경에 `KENFOLD_CODEX_KEY`를 설정하고, `~/.codex/config.toml`에 추가하세요.

```toml
[mcp_servers.kenfold]
url = "http://127.0.0.1:7077/mcp"
bearer_token_env_var = "KENFOLD_CODEX_KEY"
```

셸 환경 변수를 읽지 못하는 데스크톱 앱이나 IDE 확장에서는 대신 `http_headers = { "Authorization" = "Bearer kf_..." }`를 사용할 수 있습니다.

### OpenCode

프로젝트의 `opencode.json` 또는 `~/.config/opencode/opencode.json`에 추가하고, 실행 환경에 `KENFOLD_OPENCODE_KEY`를 설정하세요.

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

클라이언트가 Kenfold를 하위 프로세스로 실행하는 방식입니다. `make build`로 빌드한 뒤 `bin/kenfold mcp`를 실행하며, `KENFOLD_AGENT`에 에이전트 이름을 지정합니다. 데이터베이스에 직접 연결하므로 API 키를 사용하지 않습니다. `KENFOLD_DATABASE_URL`의 기본값은 Compose 데이터베이스입니다.

```sh
claude mcp add --env KENFOLD_AGENT=claude-code --transport stdio --scope user kenfold -- /abs/path/to/bin/kenfold mcp
```

```toml
[mcp_servers.kenfold]
command = "/abs/path/to/bin/kenfold"
args = ["mcp"]
env = { KENFOLD_AGENT = "codex" }
```

`make up-embed`를 사용한다면 stdio 프로세스에도 `KENFOLD_EMBED_URL=http://127.0.0.1:11435/v1`을 설정하세요.

### ChatGPT·claude.ai·다른 기기

터널이나 리버스 프록시를 통한 공개 HTTPS 주소가 필요합니다.

```sh
KENFOLD_PUBLIC_URL=https://kenfold.example.com make up
docker compose exec kenfold /usr/local/bin/kenfold password
```

이 설정은 내장 OAuth 2.1 서버를 활성화합니다. 소유자 비밀번호로 각 클라이언트를 승인하며, 읽기 전용 또는 읽기·쓰기 권한을 선택합니다. 같은 비밀번호로 대시보드에도 로그인합니다. Tailscale Funnel, Cloudflare Tunnel, Caddy 구성은 [원격 배포 안내](docs/ko/deploy.md)를 참고하세요.

## 세션 훅

MCP 도구는 모델이 호출하기로 결정할 때만 실행됩니다. Claude Code와 Codex의 훅을 설정하면 메모리를 자동으로 활용할 수 있습니다.

- **세션 시작:** 현재 저장소의 인계 메모, 최근 세션, 프로젝트 지식, 사용자 선호를 불러옵니다.
- **세션 중:** 사용자 요청, 최종 응답, Claude Code의 컨텍스트 압축 요약을 로컬 로그에 기록합니다. 기본 위치는 `~/.local/state/kenfold`이며, 소유자만 읽을 수 있고 비밀정보는 가립니다. 이 단계에서는 네트워크 호출이 없습니다.
- **세션 종료:** 로그를 짧은 `episodic` 메모리로 저장합니다. 서버가 꺼져 있으면 다음 세션 시작 때 다시 전송합니다.

Git 저장소 밖의 세션은 기록하지 않습니다. 훅 오류는 경고로 알리고 에이전트 작업을 계속 진행합니다.

대기 중인 요약은 원래 서버 URL, 클라이언트 이름, 설정된 인증정보에 묶입니다. 다른 에이전트나 서버로 보내지 않으며, 소유자를 확인할 수 없는 구형 대기 파일은 경고와 함께 보관합니다. 대기 파일은 30일 후 만료됩니다.

```sh
make build
mkdir -p ~/.config/kenfold && (umask 077; make -s key AGENT=claude-code > ~/.config/kenfold/claude-code.key)
bin/kenfold hook config claude-code --key-file ~/.config/kenfold/claude-code.key
```

출력된 `hooks` 객체를 `~/.claude/settings.json`에 병합하세요. Codex에서는 `codex`용 키와 `bin/kenfold hook config codex`를 사용하고 결과를 `~/.codex/hooks.json`에 저장합니다. Codex에서 `/hooks`를 한 번 실행해 새 훅을 검토·승인하세요.

`--no-capture`는 세션 기록 없이 시작 시 메모리만 불러옵니다. `--no-refs`는 시작 시 코드 참조 검사를 끕니다.

## 메모리 추출

채팅 모델을 설정하면 세션 요약에서 오래 유지할 프로젝트 규칙, 코드 사실, 사용자 선호를 추출해 제안합니다. 일회성 작업과 붙여 넣은 문서의 내용은 제외합니다.

```sh
make up-extract                # 검색 모델 + qwen3.5:4b, 최초 총 다운로드 약 5.2 GB
make review                    # 제안된 메모리를 승인·거절하거나 기존 메모리와 교체
```

추출 결과는 `proposed` 상태이며, CLI 또는 [대시보드](#대시보드)에서 승인하기 전에는 에이전트에 제공하지 않습니다. 각 제안에는 출처 인용문과 유사한 기존 메모리가 표시됩니다. 교체를 승인하면 기존 메모리는 이력으로 남습니다. 거절한 내용은 다시 제안하지 않습니다.

메모리 유형을 생략하면 같은 모델이 분류합니다. CPU에서 `remember` 호출에 약 6초가 추가되며, 15초 안에 끝나지 않으면 기본 유형을 사용합니다. 백그라운드 세션 추출은 CPU에서 약 10~30초가 걸립니다.

성능 코어와 효율 코어가 나뉜 CPU에서는 `THREADS`를 성능 코어 수로 설정하세요. 기본값은 4이며, 예를 들어 `make up-extract THREADS=6`으로 바꿀 수 있습니다. `KENFOLD_CHAT_URL`과 `KENFOLD_CHAT_MODEL`로 OpenAI 호환 채팅 API도 사용할 수 있습니다. [내부 추출 평가](internal/extract/testdata/RESULTS.md)(영문)는 `make eval`로 실행합니다.

`KENFOLD_EXTRACT_POLICY=auto`는 명시적으로 서술되었고 기존 메모리와 유사하지 않은 추출 결과를 자동 활성화합니다. 사용자 선호는 항상 검토합니다. 붙여 넣은 내용의 지시가 메모리에 섞일 수 있으므로 기본값인 `propose`는 모든 추출 결과를 검토하도록 합니다.

## 메모리 정리

채팅 모델을 설정하면 중복·모순 메모리 정리와 오래된 세션 요약을 제안합니다.

- **중복:** 한 메모리가 다른 메모리의 내용을 모두 포함하면 더 많은 정보를 가진 쪽을 남깁니다.
- **모순:** 서로 충돌하는 내용은 더 최근 메모리를 남기는 안을 제안합니다.
- **오래된 세션:** 한 프로젝트에 30일이 지난 세션 요약이 6개 이상이면 가장 오래된 최대 10개를 하나의 요약으로 묶습니다.

소유자가 적용하기 전에는 아무것도 바뀌지 않습니다. 기존 메모리는 이력으로 보존합니다. 중복·모순 정리에서는 내용을 새로 작성하지 않고 기존 메모리 하나를 그대로 남깁니다.

```sh
bin/kenfold consolidate list
bin/kenfold consolidate apply <id>    # 또는 reject <id>
bin/kenfold consolidate run           # 정기 실행을 기다리지 않고 지금 검사
```

모델은 후보 쌍의 순서를 바꿔 두 번 판단하고 결과가 일치할 때만 제안합니다. 세션을 묶은 요약의 파일명·식별자·숫자는 원문에 있어야 합니다. 작업자는 15분마다 실행하며 추출 작업을 우선합니다. `KENFOLD_CONSOLIDATE=false`로 끌 수 있습니다. [설계](docs/adr/0004-consolidation.md)와 [내부 평가](internal/consolidate/testdata/RESULTS.md)는 영문으로 제공됩니다.

## 코드 참조

코드에 관한 메모리는 코드가 바뀌면 오래된 정보가 됩니다. Kenfold는 메모리에 언급된 파일과 심볼을 기록하고, 세션 시작 시 훅이 저장소 `HEAD`에서 해당 내용을 검사합니다. 심볼이 바뀌거나 파일·심볼이 사라지면 `stale`로 표시하고 검색 순위를 낮춥니다.

검사는 커밋된 코드만 대상으로 합니다. 파일에 미커밋 변경이 있으면 기준 해시를 고정하지 않습니다. 기준 커밋을 포함하지 않는 오래된 체크아웃이나 다른 브랜치에서는 변경을 보고하지 않습니다. 파일 자체의 변경만으로는 메모리를 오래된 것으로 판정하지 않습니다.

```sh
bin/kenfold refs sync                 # KENFOLD_API_KEY 또는 --key-file 사용
bin/kenfold refs status
bin/kenfold memory list --stale
```

커밋마다 검사하려면 `.git/hooks/post-commit` 등에 다음을 추가할 수 있습니다.

```sh
#!/bin/sh
/abs/path/to/bin/kenfold refs sync --quiet --key-file ~/.config/kenfold/claude-code.key || true
```

`make build`는 Go, TypeScript/TSX, JavaScript, Python, Rust, Java, Kotlin, Ruby, C#, PHP, C, C++, Swift, Scala, Bash의 심볼 탐색을 지원합니다. `GRAMMARS=...`로 포함할 언어를 변경할 수 있으며, 나머지 파일은 파일 단위로 추적합니다.

## 메모리 동작 방식

- **프로젝트 식별:** Git 원격 URL을 정규화합니다. `git@github.com:Org/Repo.git`과 `https://github.com/org/repo`는 같은 프로젝트입니다. 기기마다 다른 로컬 경로는 거절합니다. 프로젝트를 생략한 메모리는 사용자 전체 범위로 모든 프로젝트에서 조회됩니다.
- **저장과 교체:** `remember`는 독립적으로 이해할 수 있는 내용을 저장합니다. 대소문자·공백·끝 문장부호만 다른 중복은 기존 메모리를 반환합니다. `supersedes`로 교체하면 기존 내용을 이력으로 남깁니다. 유사 메모리는 `similar`로 반환하며 자동 병합하지 않습니다.
- **비밀정보 차단:** 일반적인 API 키, 토큰, 개인키, URL이나 대입문 속 비밀번호, JWT를 포함한 쓰기는 거절합니다. `bin/kenfold scan`으로 기존 메모리를 검사하고 `--redact`로 비밀정보를 가릴 수 있습니다.
- **사용자 선호:** 에이전트가 작성한 `preference`는 승인 전까지 제공하지 않습니다. 승인하면 `trust = user`가 됩니다.
- **인계:** `handoff`는 프로젝트마다 하나의 활성 메모를 유지합니다. `resume`은 이를 반환하고 누가 이어받았는지 기록합니다. `get_context`는 아직 이어받지 않은 인계만 제공합니다.
- **검색:** `recall`은 전문·벡터 검색 순위를 결합하고, 연결된 메모리를 확장하며, 설정된 경우 재순위화를 수행합니다. 최근 작업을 묻는 질문에는 최신 메모리를 우선하고, 코드가 오래된 메모리는 순위를 낮춥니다. 모델 장애 시 사용 가능한 단계로 검색을 계속합니다.

```sh
make build
bin/kenfold memory review
bin/kenfold memory list --status proposed
bin/kenfold memory approve <id>
bin/kenfold memory forget <id> --reason "오래된 정보"
```

## 대시보드

`http://127.0.0.1:7077/dashboard/`에서 소유자 비밀번호로 로그인합니다. 처음에는 다음 명령으로 비밀번호를 설정하세요.

```sh
docker compose exec kenfold /usr/local/bin/kenfold password
```

- **Review:** 추출 결과와 사용자 선호를 오래된 순서대로 검토하고 승인·거절·교체합니다.
- **Consolidation:** 중복·모순 정리와 오래된 세션 요약 제안을 적용하거나 거절합니다.
- **Memories:** 메모리를 검색하고 범위·유형·상태별로 조회합니다. 출처, 버전, 연결, 코드 참조를 확인하고 삭제 처리할 수 있습니다.
- **Clients:** API 키와 OAuth 클라이언트의 최근 사용을 확인하고 철회합니다. 새 키는 서버에서만 발급합니다.

기본 대시보드는 루프백 호스트 이름에서만 응답하고 프록시·터널을 거친 요청을 거절합니다. SSH 포트 포워딩과 `KENFOLD_DASHBOARD=remote`는 [배포 문서](docs/ko/deploy.md#대시보드는-기본적으로-로컬에서만-제공됩니다)를 참고하세요. JavaScript를 사용하지 않으며, 변경 요청에는 세션의 CSRF 토큰이 필요합니다. 세션은 2시간 미사용, 최대 12시간 경과, 비밀번호 변경 또는 서버 재시작 시 종료됩니다.

## 백업과 서버 이전

```sh
docker compose exec -T kenfold /usr/local/bin/kenfold export > kenfold-backup.jsonl
bin/kenfold export --out backup.jsonl --scope github.com/org/repo --active
bin/kenfold import --dry-run backup.jsonl && bin/kenfold import backup.jsonl
```

아카이브에는 메모리, 이력, 출처, 타임스탬프, 연결, 코드 참조가 포함됩니다. 임베딩, API 키, OAuth 클라이언트는 제외합니다. 대상 서버는 자체 모델로 다시 임베딩합니다. 기존 메모리는 유지하므로 가져오기를 반복해도 중복 생성되지 않습니다. 불완전한 아카이브나 비밀정보가 포함된 메모리가 있으면 전체 가져오기를 거절합니다.

`--out`으로 만든 파일은 소유자만 읽을 수 있습니다. 아카이브는 메모리를 평문으로 담으므로 데이터베이스와 같은 수준으로 보관하세요. 셸 리다이렉션으로 만드는 파일의 권한은 셸의 `umask`를 따릅니다.

## 검색 품질

3개 프로젝트의 메모리 110개, 한국어·영어 쿼리 67개로 구성된 내부 평가입니다. `make eval-search`로 실행할 수 있습니다. Recall@5는 질문에 답하는 메모리 중 상위 5개 결과에서 찾은 비율입니다.

| 구성 | 개발 세트 43개 | 홀드아웃 24개 | 검색 시간 |
|---|---|---|---|
| `make up` — 전문 검색 | 0.65 | 0.44 | 수 ms |
| 2단계 하이브리드 검색 | 0.85 | 0.88 | 약 25 ms |
| `make up-embed RERANK=0` | 0.90 | 0.90 | 약 25 ms |
| `make up-embed` — 재순위화 포함 | **0.96** | **0.96** | 약 1.8초 |

Apple M5 CPU에서 모델을 미리 로드한 상태로 측정했습니다. 개발 세트는 파이프라인 조정에 사용했고, 홀드아웃은 순위 계산 코드를 보지 않은 별도 작성자가 이후에 만들었습니다. 작은 합성 데이터셋으로 동작을 확인한 결과이며 다른 시스템과의 성능 비교는 아닙니다. [상세 결과](internal/retrieve/testdata/RESULTS.md)(영문)를 참고하세요.

## MCP 도구

| 도구 | 용도 |
|---|---|
| `get_context` | 작업 시작 시 선호, 인계, 최근 세션, 프로젝트 지식, 관련 메모리 조회 |
| `remember` | 오래 유지할 메모리 저장 및 기존 메모리 교체 |
| `recall` | 자연어 검색 |
| `handoff` | 다음 에이전트나 세션에 작업 인계 |
| `resume` | 최신 인계 메모를 이어받음 |
| `forget` | 메모리 논리 삭제 |

인자와 반환값은 [MCP 도구 명세](docs/ko/mcp-tools.md)를 참고하세요.

## 보안

- 포트는 기본적으로 `127.0.0.1`에만 공개합니다. 원격 접속은 TLS 터널·리버스 프록시와 `KENFOLD_PUBLIC_URL`을 사용합니다.
- `/mcp`는 기본적으로 API 키 또는 OAuth 액세스 토큰이 필요합니다. 키와 토큰은 SHA-256 해시만 저장합니다. `bin/kenfold key revoke <prefix>` 또는 `bin/kenfold oauth revoke <grant>`로 철회합니다.
- Kenfold는 단일 소유자용입니다. 모든 유효한 키와 승인된 OAuth 클라이언트는 모든 메모리를 읽을 수 있습니다. 프로젝트 범위는 접근 격리 경계가 아닙니다. OAuth 쓰기 권한은 별도로 승인하며 기본 선택은 읽기 전용입니다.
- 소유자 비밀번호는 Argon2id로 저장합니다. 동의 페이지와 대시보드에서 합산 5회 실패하면 양쪽 로그인이 15분간 잠깁니다.
- `/mcp`와 `/api/v1`은 Host 허용 목록과 교차 사이트 요청 차단을 적용합니다. `/healthz`와 `/readyz`는 인증 없이 상태만 제공합니다.
- `KENFOLD_AUTH=none`은 인증을 끄며, 작성자는 클라이언트가 전달한 이름으로 기록됩니다.
- 비밀정보 필터는 알려진 형식과 비밀 필드에 지정된 무작위 형태의 값을 찾는 패턴 기반 필터입니다. 모든 비밀정보를 식별하는 것은 아니므로 에이전트에 비밀정보를 저장하지 않도록 해야 합니다.

## CLI

```text
kenfold serve                        HTTP 서버 실행
kenfold healthcheck                  서버·DB 스키마 준비 상태 확인
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
kenfold scan [--redact]              저장된 비밀정보 탐지·가림
kenfold hook [--no-capture] [--no-refs]
kenfold hook config <claude-code|codex> [--key-file F]
kenfold version
```

## 설정

| 환경 변수 | 기본값·설명 |
|---|---|
| `KENFOLD_HTTP_ADDR` | `127.0.0.1:7077` |
| `KENFOLD_DATABASE_URL` | `postgres://kenfold:kenfold@127.0.0.1:54329/kenfold?sslmode=disable` — 로컬 Compose DB |
| `KENFOLD_DATABASE_PASSWORD` | 미설정. PostgreSQL URL의 비밀번호를 별도로 지정하며 자동 이스케이프합니다. Compose는 `KENFOLD_DB_PASSWORD`를 전달합니다 |
| `KENFOLD_AUTO_MIGRATE` | `false`, Compose는 `true`. `serve`와 `mcp`는 오래된 스키마에서 실행을 거절합니다 |
| `KENFOLD_ALLOWED_HOSTS` | `localhost,127.0.0.1,::1` — Host 허용 목록 |
| `KENFOLD_AUTH` | `apikey` 또는 `none` |
| `KENFOLD_PUBLIC_URL` | 미설정. 경로 없는 공개 HTTPS URL. 호스트를 허용 목록에 추가하고 OAuth를 활성화합니다 |
| `KENFOLD_OAUTH` | 공개 URL 설정 시 활성화 |
| `KENFOLD_OAUTH_DCR` | `true` — 동적 클라이언트 등록 허용 |
| `KENFOLD_DASHBOARD` | `local` — 로컬 전용. `remote`는 공개 URL에서도 제공, `off`는 비활성화 |
| `KENFOLD_AGENT` | stdio 서버의 에이전트 이름 |
| `KENFOLD_EMBED_URL` | 미설정 시 전문 검색만 사용. OpenAI 호환 임베딩 API 주소 |
| `KENFOLD_EMBED_MODEL` | `bge-m3`, 반드시 1024차원 벡터 출력 |
| `KENFOLD_EMBED_NAME` | 미설정. 모델 별칭과 별도로 벡터에 기록할 이름. `make up-embed`는 `kenfold-embed`를 실행하고 `bge-m3`로 기록합니다 |
| `KENFOLD_EMBED_API_KEY` | 미설정. 임베딩 API에 전달할 Bearer 토큰 |
| `KENFOLD_EMBED_DIMENSIONS` | `false`. `true`이면 `dimensions=1024` 전달 |
| `KENFOLD_SEARCH_MAX_DISTANCE` | `0.55` — 벡터 검색의 최대 코사인 거리 |
| `KENFOLD_RERANK_URL` | 미설정 시 재순위화 비활성화. llama.cpp, Jina, Cohere, Voyage 등의 `/rerank` API |
| `KENFOLD_RERANK_MODEL` | `bge-reranker-v2-m3` |
| `KENFOLD_RERANK_API_KEY` | 미설정. 재순위화 API에 전달할 Bearer 토큰 |
| `KENFOLD_CHAT_URL` | 미설정 시 추출 비활성화. OpenAI 호환 채팅 API 주소 |
| `KENFOLD_CHAT_MODEL` | `qwen3.5:4b`. `make up-extract`는 스레드 수를 제한한 별칭 `kenfold-extract` 사용 |
| `KENFOLD_CHAT_API_KEY` | 미설정. 채팅 API에 전달할 Bearer 토큰 |
| `KENFOLD_CHAT_REASONING` | `none`을 `reasoning_effort`로 전달. 이 필드를 거절하는 서버에는 `omit` 사용 |
| `KENFOLD_EXTRACT` | 채팅 모델 설정 시 활성화 |
| `KENFOLD_EXTRACT_POLICY` | `propose` — 모두 검토. `auto`는 조건을 충족한 추출 결과 자동 활성화 |
| `KENFOLD_CLASSIFY` | 채팅 모델 설정 시 활성화. 유형이 생략된 메모리 분류 |
| `KENFOLD_CONSOLIDATE` | 채팅 모델 설정 시 활성화. 중복·모순 정리와 오래된 세션 요약 제안 |
| `KENFOLD_LOG_LEVEL` | `info` |
| `KENFOLD_UPDATE_CHECK` | `true` — 릴리스 빌드의 새 버전 확인. 자동 설치·재시작 없음 |

훅과 `kenfold refs sync`는 `KENFOLD_URL`(기본 `http://127.0.0.1:7077/mcp`) 및 `KENFOLD_API_KEY`를 읽습니다. `--key-file`이 있으면 해당 파일의 키를 사용합니다. 훅의 상태 디렉터리는 `KENFOLD_STATE_DIR`(기본 `~/.local/state/kenfold`)로 변경합니다.

다른 1024차원 임베딩 모델로 변경할 때는 마이그레이션이 필요하지 않습니다. 현재 모델로 생성한 벡터끼리만 비교하고 나머지는 백그라운드에서 다시 임베딩합니다. `kenfold reindex`로 직접 실행할 수도 있습니다. 다른 차원으로 변경하려면 마이그레이션이 필요합니다.

## 개발

```sh
make test               # 단위 테스트 + stdio E2E, DB 불필요
make check              # gofmt, vet, race 테스트, 빌드, 취약점 검사
make test-integration   # Compose PostgreSQL의 별도 테스트 DB 사용
make lint               # gofmt + go vet
make eval               # 추출·정리 품질 평가, up-extract 필요
make eval-search        # 검색 단계별 평가, up-embed 필요
make bench              # LoCoMo + LongMemEval_S 일부 평가
make build              # bin/kenfold 빌드
make logs               # 서버 로그
make down               # 스택 종료, 데이터 볼륨 유지
```

`make check`에 통합 테스트를 포함하려면 `KENFOLD_TEST_DATABASE_URL`을 폐기 가능한 PostgreSQL 18 + pgvector 데이터베이스로 설정하세요. 테스트는 데이터를 비우고 패키지를 순차 실행합니다. GitHub Actions는 별도 DB로 검사를 실행하고 프로덕션 컨테이너를 빌드합니다.

공개 벤치마크는 외부 API 비용이 발생할 수 있습니다. `KENFOLD_BENCH_CHAT_URL`, `KENFOLD_BENCH_API_KEY`와 예산·입출력 단가를 설정해야 합니다. `KENFOLD_BENCH_REASONING=none`을 사용하고, 단가는 읽기·채점 모델 모두의 컨텍스트 구간별 요금을 포함해야 합니다. 사용량이 누락되면 호출을 중단하고 미완료 결과를 `PARTIAL`로 보존합니다.

재시작 가능한 평가에는 문항별 점수 캐시와 사용량 저널을 함께 보존해야 합니다. 데이터, 코드, 모델 가중치·설정이 일치하는 실행 식별자가 필요하며, 불확실한 진행 중 호출은 자동 재시도하지 않습니다. 예산은 기본적으로 실행별로 적용됩니다. 상세 변수, 복구 절차, 추출 체크포인트는 [영문 개발 안내](README.md#development)를 참고하세요. [복구 검사](internal/bench/testdata/RESULTS.md#free-recovery-verification--no-quality-scores)는 운영 검증이며 QA 품질 점수가 아닙니다.

### 디렉터리 구조

```text
cmd/kenfold/          CLI, 서버 실행, 훅, 관리 명령, E2E 테스트
internal/mcpserver/  MCP 도구 스키마와 핸들러
internal/store/      PostgreSQL 저장, 하이브리드 검색, 메모리 연결
internal/retrieve/   검색·확장·재순위화 파이프라인과 평가 데이터
internal/coderef/    코드 참조 추출, Git·tree-sitter 검사, 동기화
internal/restapi/    /api/v1 REST API
internal/dashboard/ 소유자용 서버 렌더링 대시보드
internal/hook/      Claude Code·Codex 세션 훅
internal/extract/   메모리 추출·유형 분류
internal/consolidate/ 메모리 정리 제안과 평가
internal/bench/     LoCoMo·LongMemEval 평가 도구
internal/secrets/   인증정보 탐지·가림
internal/oauth/     OAuth 2.1 서버
internal/config/    환경 변수 설정
migrations/         바이너리에 포함되는 SQL 마이그레이션
deploy/             원격 접속용 리버스 프록시 예제
docs/               배포 안내, 도구 명세, 설계 결정 기록
```

## 로드맵

| 단계 | 범위 |
|---|---|
| **0** ✅ | 스키마, MCP 계약, 프로젝트 골격, Compose |
| **1** ✅ | 저장·하이브리드 검색, API 키, CLI, 에이전트 연결 |
| **2** ✅ | 비밀정보 필터, 중복·모순 힌트, 자동 세션 요약 훅 |
| **2b** ✅ | 검토 기반 메모리 추출, 유형 분류 |
| **3** ✅ | 재순위화, 메모리 연결 확장, 최신성·코드 변경 반영, REST API |
| **4** ✅ | OAuth 2.1, 원격 배포. SDK 클라이언트로 HTTPS E2E 검증했으며 실제 ChatGPT 연결은 미검증. 객체 저장소·OpenAI 호환 프록시는 [보류](docs/adr/0003-remote-access-and-oauth.md) |
| **5** | 내보내기·가져오기 ✅, 대시보드 ✅, 메모리 정리 ✅. 공개 벤치마크 검증은 미완료 |
