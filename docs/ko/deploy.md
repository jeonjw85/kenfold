# 원격 접속

[English](../deploy.md) | [한국어](deploy.md)

기본 수신 주소는 `127.0.0.1`, 같은 기기의 에이전트는 이 주소로 연결

다음 클라이언트에는 외부에서 접근 가능한 HTTPS URL 필요

- 제공업체의 클라우드에서 실행되는 ChatGPT와 claude.ai의 웹 및 모바일 클라이언트
- 다른 기기에서 실행하는 에이전트

TLS는 터널이나 리버스 프록시에서 처리하고 Kenfold에 공개 URL 설정

```sh
KENFOLD_PUBLIC_URL=https://kenfold.example.com make up    # 또는 make up-embed / up-extract
```

`KENFOLD_PUBLIC_URL` 설정 시 해당 호스트를 허용 목록에 추가하고 내장 OAuth 2.1 서버 활성화

ChatGPT와 claude.ai는 OAuth로 연결, API 키를 사용하는 에이전트는 로컬 주소와 공개 URL 모두에서 기존 방식으로 연결 가능

## 공개 전 확인사항

공개 URL은 전체 메모리 저장소로 연결되며 메모리 접근에는 인증 필요

1. **소유자 비밀번호 설정**

   OAuth 클라이언트 승인과 [대시보드](#대시보드-로컬-접근) 로그인에 사용, 다른 곳에서 쓰지 않는 12자 이상의 비밀번호 필요

   ```sh
   docker compose exec kenfold /usr/local/bin/kenfold password
   ```

   동의 페이지와 대시보드에서 합산 5회 실패 시 양쪽 로그인 15분 잠금

   `make logs`에 비밀번호 오류나 잠금 기록이 반복되면 추측 공격 여부 확인

2. **`KENFOLD_AUTH=apikey` 유지**

   인증 비활성화 시 OAuth 서버 시작 불가

3. **필요한 권한만 승인**

   - 기본 선택은 읽기 전용, 메모리를 저장할 클라이언트에만 쓰기 허용
   - 쓰기 권한으로 다른 에이전트가 전달받는 메모리 변경 가능
   - 읽기 권한만 있어도 모든 프로젝트의 메모리 조회 가능

4. **직접 시작한 연결만 승인**

   - 동의 페이지의 클라이언트 ID와 승인 후 이동할 호스트 확인
   - ChatGPT는 `chatgpt.com`, claude.ai는 `claude.ai` 사용
   - 클라이언트 이름은 누구나 지정할 수 있으므로 이름만 보고 승인하지 않음

5. **데이터베이스 백업**

   메모리 보존이 필요하면 `pgdata` 볼륨 백업

   메모리 내보내기와 가져오기는 [한국어 README](../../README.ko.md#백업과-서버-이전) 참고

릴리스 전에는 `KENFOLD_TEST_DATABASE_URL`을 폐기 가능한 PostgreSQL 18 + pgvector 데이터베이스로 지정하고 `make check` 실행

- 포맷 검사, vet, 캐시를 사용하지 않는 race 테스트, 바이너리 빌드, Go 취약점 검사 수행
- 변수 미설정 시 DB 통합 테스트 생략
- 테스트는 데이터를 비우므로 운영 메모리 DB와 분리 필요

Compose는 `kenfold healthcheck` 사용

`up --wait`는 HTTP 서버가 현재 스키마를 갖춘 DB에 연결할 때까지 대기

- `/healthz`: 프로세스 상태 검사
- `/readyz`: DB와 스키마 준비 상태 검사
- 선택적 모델 장애는 전문 검색으로 전환 가능하므로 준비 실패에서 제외

이 릴리스는 기본 Compose 구성과 같이 서버 인스턴스 하나로 실행

- OAuth 동의 서명, JWT 재사용 방지 상태, 대시보드 세션은 프로세스 메모리에 보관
- 재시작 시 대시보드 세션 종료
- 다중 인스턴스는 작업자 임대 만료 후 추출 후보 쓰기에도 추가 조정 필요
- 자동 테스트는 SDK 클라이언트를 사용하므로 실제 공개 URL에서 ChatGPT와 claude.ai 연결 확인 필요

DB 비밀번호 설정

- 새 DB의 `KENFOLD_DB_PASSWORD`에는 URL 특수문자 사용 가능
- Compose가 비밀번호를 별도로 전달하고 Kenfold가 이스케이프 처리
- 호스트 CLI에서는 같은 값을 `KENFOLD_DATABASE_PASSWORD`로 설정
- 환경 변수 변경만으로는 기존 PostgreSQL 데이터 볼륨 내부의 비밀번호 변경 불가

## 대시보드 로컬 접근

`KENFOLD_PUBLIC_URL`을 설정해도 대시보드는 기본적으로 로컬에서만 제공

`/dashboard/`는 `localhost`와 루프백 주소에만 응답하며 `X-Forwarded-For` 같은 프록시 헤더가 있으면 요청 거절

공개 URL이나 Host 헤더를 바꾸는 터널로도 접근 불가

다른 기기에서는 SSH 포트 포워딩 후 `http://127.0.0.1:7077/dashboard/`로 접속

```sh
ssh -L 7077:127.0.0.1:7077 your-server
```

- `KENFOLD_DASHBOARD=remote`: 공개 URL의 호스트에서도 대시보드 제공
- 원격 대시보드의 메모리 읽기와 삭제, 클라이언트 철회는 소유자 비밀번호로 보호
- 가능하면 `tailscale serve`나 터널 앞단의 접근 정책으로 소유자만 접근하도록 제한
- `KENFOLD_DASHBOARD=off`: 대시보드 비활성화

## 방법 A: Tailscale Funnel

[Tailscale](https://tailscale.com)이 설치된 기기에서 사용 가능, 별도 도메인 불필요

Funnel이 로컬 포트를 `https://<machine>.<tailnet>.ts.net`으로 공개하고 인증서 관리

```sh
tailscale funnel --bg 7077            # 공개 URL 출력
KENFOLD_PUBLIC_URL=https://<machine>.<tailnet>.ts.net make up
```

- `<machine>`과 `<tailnet>`은 출력된 URL의 실제 값으로 교체
- Funnel은 Compose가 Kenfold를 공개하는 `http://127.0.0.1`로 프록시
- URL에 접근할 수 있는 사람은 동의 페이지에도 접근 가능
- `tailscale funnel reset`으로 공개 중단

본인 기기끼리만 사용한다면 `tailscale serve --bg 7077`로 tailnet 내부에 공개

ChatGPT와 claude.ai는 tailnet 전용 URL에 접근 불가

## 방법 B: Cloudflare Tunnel

Cloudflare에서 관리하는 도메인 필요, 이름 있는 터널을 한 번 생성

```sh
cloudflared tunnel login
cloudflared tunnel create kenfold
cloudflared tunnel route dns kenfold kenfold.example.com
```

`~/.cloudflared/config.yml`에 아래 내용 설정

```yaml
tunnel: <tunnel id printed by create>
credentials-file: /home/you/.cloudflared/<tunnel id>.json
ingress:
  - hostname: kenfold.example.com
    service: http://127.0.0.1:7077
  - service: http_status:404
```

터널 ID, 인증 파일 경로, 도메인은 실제 값으로 교체한 뒤 터널과 Kenfold 실행

`cloudflared tunnel run`은 포그라운드로 실행되므로 Kenfold 명령은 다른 터미널에서 실행하거나 터널을 서비스로 등록

```sh
cloudflared tunnel run kenfold
```

```sh
KENFOLD_PUBLIC_URL=https://kenfold.example.com make up
```

빠른 임시 터널(`*.trycloudflare.com`)은 시작할 때마다 URL 변경

OAuth 토큰은 공개 URL에 묶이므로 URL 변경 시 클라이언트 재승인 필요

## 방법 C: 서버의 Caddy

공개 IP와 해당 IP를 가리키는 DNS 이름이 있는 서버에서 사용

[`deploy/compose.caddy.yaml`](../../deploy/compose.caddy.yaml)로 인증서를 자동 발급하고 갱신하는 [Caddy](https://caddyserver.com) 추가

외부에서 80번과 443번 포트에 접근 가능해야 함

저장소 루트에서 실행

```sh
export KENFOLD_DOMAIN=kenfold.example.com KENFOLD_PUBLIC_URL=https://kenfold.example.com
docker compose -f compose.yaml -f deploy/compose.caddy.yaml up -d --build --wait
```

네트워크에는 Caddy 포트만 공개, Kenfold는 `127.0.0.1:7077`과 Compose 내부 네트워크에 유지

다른 리버스 프록시의 사용 조건

- TLS 처리
- Host 헤더를 그대로 전달하거나 전달할 호스트를 `KENFOLD_ALLOWED_HOSTS`에 추가
- `text/event-stream` 응답 버퍼링 비활성화
- 경로 접두사 없이 호스트 루트에서 Kenfold 제공

## ChatGPT 연결

웹의 Pro, Plus, Business, Enterprise, Education 계정에서 개발자 모드 사용 가능

1. ChatGPT의 Settings 메뉴에서 Security and login을 열고 개발자 모드 활성화
2. Plugins 페이지의 `+` 버튼으로 앱을 만들고 URL에 `https://kenfold.example.com/mcp`, 인증 방식에 OAuth 지정
3. ChatGPT가 Kenfold 인증 서버를 발견한 뒤 동의 페이지 표시
   - 클라이언트 메타데이터 문서와 동적 등록 모두 지원
   - 메타데이터 문서 방식의 `private_key_jwt` 인증은 ChatGPT의 공개키로 검증
4. 동의 페이지에서 아래 항목 설정
   - 접근 권한
   - 메모리 작성자로 기록할 에이전트 이름: 기본 `chatgpt`
   - 소유자 비밀번호

ChatGPT는 쓰기 도구인 `remember`, `handoff`, `resume`, `forget`의 실행 확인 요청

`get_context`와 `recall`은 읽기 전용으로 표시

서비스 화면과 메뉴는 변경될 수 있으므로 [검증 범위](#검증-범위) 참고

## claude.ai 연결

URL이 `https://kenfold.example.com/mcp`인 사용자 지정 커넥터 추가

Claude는 메타데이터 문서로 자신을 식별하고 `claude.ai`로 돌아가는 공개 클라이언트로 연결

Kenfold가 이 방식을 지원하며, 이후 동의 페이지에서 승인

## 다른 기기 연결

Claude Code, Codex, OpenCode 등 API 키를 사용하는 에이전트는 로컬 주소 대신 공개 URL의 `/mcp`에 연결

세션 훅에는 `KENFOLD_URL` 설정, 키는 서버에서 `make key`로 관리

## 클라이언트 관리

```sh
docker compose exec kenfold /usr/local/bin/kenfold oauth clients
docker compose exec kenfold /usr/local/bin/kenfold oauth revoke <grant>
docker compose exec kenfold /usr/local/bin/kenfold password
```

위 명령은 순서대로 승인된 클라이언트와 권한 및 최근 사용 조회, 클라이언트 철회, 소유자 비밀번호 변경

철회된 토큰은 다음 요청부터 사용 불가, 비밀번호 변경 시 대시보드 세션 종료

대시보드의 `/dashboard/clients`에서도 OAuth 클라이언트와 API 키 조회 및 철회 가능

- **토큰 수명:** 액세스 토큰 1시간, 갱신 토큰 30일, 갱신 토큰은 1회만 사용 가능하며 재사용 시 해당 클라이언트 승인 철회
- **공개 URL 변경:** `KENFOLD_PUBLIC_URL` 변경 시 기존 OAuth 토큰 무효화, 클라이언트 재연결 필요
- **동적 등록:** `KENFOLD_OAUTH_DCR=false`로 비활성화, 승인되지 않은 등록은 하루 후 삭제

## 검증 범위

배포용 [`deploy/Caddyfile`](../../deploy/Caddyfile)과 Caddy 내부 CA를 사용해 로컬 `https://kenfold.localhost:8443`에서 아래 항목 확인

- 401 인증 요구와 메타데이터 탐색
- 동적 등록, 동의 페이지, PKCE 코드 교환, 갱신 토큰 교체
- 읽기와 쓰기 권한, 읽기 전용 권한으로 MCP 호출
- CLI에서 승인 철회

통합 테스트 검사 항목

- MCP Go SDK의 OAuth 클라이언트: 메타데이터 문서 및 동적 등록 방식
- `private_key_jwt` 클라이언트: 잘못된 키, 대상, 만료, 재사용 검사

ChatGPT와 Claude가 게시한 메타데이터 문서, ChatGPT의 공개키 목록은 Kenfold의 실제 조회 코드로 수용 여부 확인

Tailscale Funnel, Cloudflare Tunnel, 실제 ChatGPT와 claude.ai 계정에서의 연결은 미검증

확인에는 공개 URL과 계정 필요
