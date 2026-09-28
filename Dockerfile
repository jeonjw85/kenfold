# syntax=docker/dockerfile:1

FROM golang:1.27.1-alpine3.24 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    # The server never parses code (clients do, in the repository), so no
    # tree-sitter grammars are embedded.
    CGO_ENABLED=0 go build -trimpath -tags grammar_subset \
      -ldflags "-s -w \
        -X github.com/kenfold/kenfold/internal/buildinfo.Version=${VERSION} \
        -X github.com/kenfold/kenfold/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/kenfold/kenfold/internal/buildinfo.Date=${DATE}" \
      -o /out/kenfold ./cmd/kenfold

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/kenfold /usr/local/bin/kenfold
# Listen on all interfaces inside the container; compose publishes to host loopback only.
ENV KENFOLD_HTTP_ADDR=0.0.0.0:7077
EXPOSE 7077
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/kenfold"]
CMD ["serve"]
