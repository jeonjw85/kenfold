# 원격 접속

[English](../deploy.md) | [한국어](deploy.md)

기본적으로 Kenfold는 `127.0.0.1`에서만 수신합니다. 같은 기기의 에이전트는 이 주소를 사용하면 됩니다. 다음 클라이언트에는 외부에서 접근 가능한 HTTPS URL이 필요합니다.

- 제공업체의 클라우드에서 실행되는 ChatGPT·claude.ai 웹 및 모바일 클라이언트
- 다른 기기에서 실행하는 에이전트

Kenfold 자체는 TLS를 처리하지 않습니다. 터널이나 리버스 프록시 뒤에 배치하고 공개 URL을 설정하세요.

```sh
KENFOLD_PUBLIC_URL=https://kenfold.example.com make up    # 또는 make up-embed / up-extract
```

`KENFOLD_PUBLIC_URL`은 해당 호스트를 Host 허용 목록에 추가하고 내장 OAuth 2.1 서버를 활성화합니다. ChatGPT와 claude.ai는 OAuth로 연결합니다. API 키를 사용하는 에이전트는 로컬 주소와 공개 URL 모두에서 기존 방식으로 연결할 수 있습니다.

## 공개 전 확인사항

공개 URL은 전체 메모리 저장소로 연결됩니다. 메모리 접근에는 인증이 필요하며, 다음 사항을 확인하세요.

1. **소유자 비밀번호를 설정하세요.** OAuth 클라이언트 승인과 [대시보드](#대시보드는-기본적으로-로컬에서만-제공됩니다) 로그인에 사용합니다. 다른 곳에서 쓰지 않는 12자 이상의 비밀번호를 사용하세요.

   ```sh
   docker compose exec kenfold /usr/local/bin/kenfold password
   ```

   동의 페이지와 대시보드에서 합산 5회 틀리면 양쪽 로그인이 15분간 잠깁니다. `make logs`에 비밀번호 오류나 잠금 기록이 반복되면 추측 공격 여부를 확인하세요.
2. **기본값인 `KENFOLD_AUTH=apikey`를 유지하세요.** 인증을 끄면 OAuth 서버는 시작하지 않습니다.
3. **필요한 권한만 승인하세요.** 기본 선택은 읽기 전용입니다. 메모리를 저장해야 하는 클라이언트에만 읽기·쓰기를 허용하세요. 쓰기 권한은 다른 에이전트가 전달받는 메모리 내용을 바꿀 수 있습니다. 읽기 권한만 있어도 모든 프로젝트의 메모리를 조회할 수 있습니다.
4. **직접 시작한 연결만 승인하세요.** 동의 페이지의 클라이언트 ID와 승인 후 이동할 호스트를 확인하세요. ChatGPT는 `chatgpt.com`, claude.ai는 `claude.ai`를 사용합니다. 누구나 클라이언트 이름을 “ChatGPT”로 등록할 수 있으므로 이름만 보고 승인하지 마세요.
5. **데이터베이스를 백업하세요.** 메모리 보존이 필요하다면 `pgdata` 볼륨을 백업하세요. 메모리 내보내기·가져오기는 [한국어 README](../../README.ko.md#백업과-서버-이전)를 참고하세요.

릴리스 전에는 `KENFOLD_TEST_DATABASE_URL`을 폐기 가능한 PostgreSQL 18 + pgvector 데이터베이스로 지정하고 `make check`를 실행하세요. 포맷 검사, vet, 캐시를 사용하지 않는 race 테스트, 바이너리 빌드, Go 취약점 검사를 수행합니다. 변수를 설정하지 않으면 DB 통합 테스트는 건너뜁니다. 테스트는 데이터를 비우므로 운영 메모리 DB와 분리해야 합니다.

Compose는 `kenfold healthcheck`를 사용합니다. `up --wait`는 HTTP 서버가 현재 스키마를 갖춘 DB에 연결할 수 있을 때까지 기다립니다. `/healthz`는 프로세스 상태, `/readyz`는 DB·스키마 준비 상태를 검사합니다. 선택적 모델이 중단되어도 전문 검색으로 전환할 수 있으므로 준비 실패로 처리하지 않습니다.

이 릴리스에서는 서버 인스턴스 하나를 사용하세요. OAuth 동의 서명, JWT 재사용 방지 상태, 대시보드 세션은 프로세스 메모리에 있습니다. 재시작하면 대시보드 세션이 끝납니다. 다중 인스턴스에서는 작업자 임대가 만료된 뒤 추출 후보를 쓰는 작업에도 추가 조정이 필요합니다. 기본 Compose 구성은 인스턴스 하나를 실행합니다. 자동 테스트는 SDK 클라이언트를 사용하므로 실제 공개 URL에서 ChatGPT·claude.ai 연결도 확인하세요.

새 DB의 `KENFOLD_DB_PASSWORD`에는 URL 특수문자를 사용할 수 있습니다. Compose가 비밀번호를 따로 전달하고 Kenfold가 이스케이프합니다. 호스트 CLI에서는 같은 값을 `KENFOLD_DATABASE_PASSWORD`로 설정하세요. 환경 변수를 바꾸는 것만으로 기존 PostgreSQL 데이터 볼륨 내부의 비밀번호가 변경되지는 않습니다.

## 대시보드는 기본적으로 로컬에서만 제공됩니다

`KENFOLD_PUBLIC_URL`을 설정해도 대시보드는 공개되지 않습니다. `/dashboard/`는 `localhost`와 루프백 주소에만 응답하며 `X-Forwarded-For` 같은 프록시 헤더가 있으면 요청을 거절합니다. 따라서 공개 URL이나 Host 헤더를 바꾸는 터널로도 접근할 수 없습니다.

다른 기기에서 사용하려면 SSH 포트 포워딩 후 `http://127.0.0.1:7077/dashboard/`를 여세요.

```sh
ssh -L 7077:127.0.0.1:7077 your-server
```

`KENFOLD_DASHBOARD=remote`는 공개 URL의 호스트에서도 대시보드를 제공합니다. 이때 인터넷에서 메모리를 읽거나 클라이언트를 철회하고 메모리를 삭제 처리하는 기능은 소유자 비밀번호로 보호됩니다. 가능하면 `tailscale serve`나 터널 앞단의 접근 정책으로 본인만 접근하도록 하세요. `KENFOLD_DASHBOARD=off`는 대시보드를 끕니다.

## 방법 A: Tailscale Funnel

[Tailscale](https://tailscale.com)이 설치된 기기에서 사용할 수 있으며 별도 도메인이 필요하지 않습니다. Funnel이 로컬 포트를 `https://<machine>.<tailnet>.ts.net`으로 공개하고 인증서를 관리합니다.

```sh
tailscale funnel --bg 7077            # 공개 URL 출력
KENFOLD_PUBLIC_URL=https://<machine>.<tailnet>.ts.net make up
```

`<machine>`과 `<tailnet>`은 출력된 URL의 실제 값으로 바꾸세요. Funnel은 Compose가 Kenfold를 공개하는 `http://127.0.0.1`로 프록시합니다. URL에 접근할 수 있는 사람은 동의 페이지에도 접근할 수 있습니다. `tailscale funnel reset`으로 공개를 중단합니다.

본인 기기끼리만 사용한다면 `tailscale serve --bg 7077`로 인터넷 대신 tailnet 내부에 공개할 수 있습니다. ChatGPT와 claude.ai는 tailnet 전용 URL에 접근할 수 없습니다.

## 방법 B: Cloudflare Tunnel

Cloudflare에서 관리하는 도메인이 필요합니다. 이름 있는 터널을 한 번 생성하세요.

```sh
cloudflared tunnel login
cloudflared tunnel create kenfold
cloudflared tunnel route dns kenfold kenfold.example.com
```

`~/.cloudflared/config.yml`에 다음을 설정합니다.

```yaml
tunnel: <tunnel id printed by create>
credentials-file: /home/you/.cloudflared/<tunnel id>.json
ingress:
  - hostname: kenfold.example.com
    service: http://127.0.0.1:7077
  - service: http_status:404
```

터널 ID, 인증 파일 경로, 도메인은 실제 값으로 바꾸세요. 이어서 터널과 Kenfold를 실행합니다. `cloudflared tunnel run`은 포그라운드로 실행되므로 Kenfold 명령은 다른 터미널에서 실행하거나 터널을 서비스로 등록하세요.

```sh
cloudflared tunnel run kenfold
```

```sh
KENFOLD_PUBLIC_URL=https://kenfold.example.com make up
```

빠른 임시 터널(`*.trycloudflare.com`)은 시작할 때마다 URL이 바뀝니다. OAuth 토큰은 공개 URL에 묶이므로 URL이 바뀌면 클라이언트를 다시 승인해야 합니다.

## 방법 C: 서버의 Caddy

공개 IP와 해당 IP를 가리키는 DNS 이름이 있는 서버에서 사용할 수 있습니다. [`deploy/compose.caddy.yaml`](../../deploy/compose.caddy.yaml)은 인증서를 자동 발급·갱신하는 [Caddy](https://caddyserver.com)를 추가합니다. 외부에서 80·443 포트에 접근할 수 있어야 합니다.

저장소 루트에서 실행하세요.

```sh
export KENFOLD_DOMAIN=kenfold.example.com KENFOLD_PUBLIC_URL=https://kenfold.example.com
docker compose -f compose.yaml -f deploy/compose.caddy.yaml up -d --build --wait
```

네트워크에는 Caddy의 포트만 공개합니다. Kenfold는 `127.0.0.1:7077`과 Compose 내부 네트워크에 유지됩니다.

다른 리버스 프록시도 다음 조건을 충족하면 사용할 수 있습니다.

- TLS를 처리합니다.
- Host 헤더를 그대로 전달하거나, 전달할 호스트를 `KENFOLD_ALLOWED_HOSTS`에 추가합니다.
- `text/event-stream` 응답을 버퍼링하지 않습니다.
- Kenfold를 경로 접두사 아래가 아니라 호스트의 루트에서 제공합니다.

## ChatGPT 연결

웹의 Pro, Plus, Business, Enterprise, Education 계정에서는 개발자 모드를 사용할 수 있습니다.

1. ChatGPT의 **Settings → Security and login**에서 개발자 모드를 켭니다.
2. Plugins 페이지의 `+` 버튼으로 앱을 만들고 URL에 `https://kenfold.example.com/mcp`, 인증 방식에 OAuth를 지정합니다.
3. ChatGPT가 Kenfold의 인증 서버를 발견하고 동의 페이지를 엽니다. Kenfold는 클라이언트 메타데이터 문서와 동적 등록을 모두 지원합니다. 메타데이터 문서 방식의 `private_key_jwt` 인증은 ChatGPT가 게시한 공개키로 검증합니다.
4. 동의 페이지에서 다음을 설정합니다.
   - 접근 권한
   - 메모리 작성자로 기록할 에이전트 이름: 기본 `chatgpt`
   - 소유자 비밀번호

ChatGPT는 쓰기 도구인 `remember`, `handoff`, `resume`, `forget`의 실행 확인을 요청합니다. `get_context`, `recall`은 읽기 전용으로 표시됩니다. 서비스의 화면과 메뉴는 변경될 수 있으며, 아래의 [검증 범위](#검증-범위)도 확인하세요.

## claude.ai 연결

URL이 `https://kenfold.example.com/mcp`인 사용자 지정 커넥터를 추가하세요. Claude는 클라이언트 메타데이터 문서로 자신을 식별합니다. `claude.ai`로 돌아가는 공개 클라이언트이며 Kenfold가 지원합니다. 이후 ChatGPT와 같은 방식으로 동의 페이지에서 승인합니다.

## 다른 기기 연결

API 키를 사용하는 에이전트는 `http://127.0.0.1:7077/mcp` 대신 공개 URL의 `/mcp`에 연결합니다. Claude Code, Codex, OpenCode 모두 같은 방식입니다. 세션 훅에는 `KENFOLD_URL`을 설정하세요. 키는 기존과 같이 서버에서 `make key`로 관리합니다.

## 클라이언트 관리

```sh
docker compose exec kenfold /usr/local/bin/kenfold oauth clients
docker compose exec kenfold /usr/local/bin/kenfold oauth revoke <grant>
docker compose exec kenfold /usr/local/bin/kenfold password
```

각각 승인된 클라이언트·권한·최근 사용 조회, 클라이언트 철회, 소유자 비밀번호 변경입니다. 철회된 토큰은 다음 요청부터 사용할 수 없으며 비밀번호를 바꾸면 대시보드 세션이 종료됩니다. 대시보드의 `/dashboard/clients`에서도 OAuth 클라이언트와 API 키를 조회·철회할 수 있습니다.

- **토큰 수명:** 액세스 토큰은 1시간, 갱신 토큰은 30일입니다. 갱신 토큰은 한 번만 사용할 수 있고 이미 사용한 토큰이 다시 제출되면 해당 클라이언트의 승인을 철회합니다.
- **공개 URL 변경:** `KENFOLD_PUBLIC_URL`이 바뀌면 기존 OAuth 토큰은 유효하지 않습니다. 클라이언트를 다시 연결해야 합니다.
- **동적 등록:** `KENFOLD_OAUTH_DCR=false`로 끌 수 있습니다. 승인되지 않은 등록은 하루 후 삭제합니다.

## 검증 범위

배포용 [`deploy/Caddyfile`](../../deploy/Caddyfile)과 Caddy 내부 CA를 사용해 로컬 `https://kenfold.localhost:8443`에서 다음을 확인했습니다.

- 401 인증 요구와 메타데이터 탐색
- 동적 등록, 동의 페이지, PKCE 코드 교환, 갱신 토큰 교체
- 읽기·쓰기 권한 및 읽기 전용 권한으로 MCP 호출
- CLI에서 승인 철회

통합 테스트에서는 다음을 검사합니다.

- MCP Go SDK의 OAuth 클라이언트: 메타데이터 문서 및 동적 등록 방식
- `private_key_jwt` 클라이언트: 잘못된 키, 대상, 만료, 재사용 검사

ChatGPT·Claude가 게시한 메타데이터 문서와 ChatGPT의 공개키 목록은 Kenfold의 실제 메타데이터 조회 코드로 읽고 수용 여부를 확인했습니다.

Tailscale Funnel, Cloudflare Tunnel, 실제 ChatGPT·claude.ai 계정에서의 연결은 검증하지 않았습니다. 공개 URL과 계정이 있어야 확인할 수 있습니다.
