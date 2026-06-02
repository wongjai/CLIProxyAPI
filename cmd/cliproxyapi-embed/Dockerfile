# Dockerfile for the cliproxyapi-embed variant of CLIProxyAPI.
#
# This image bundles the standard cliproxy SDK runtime (all built-in
# translators included via sdk/translator/builtin) PLUS an additional
# POST /v1/embeddings route backed by Google embedding APIs. It is a
# drop-in replacement for the upstream cli-proxy-api image.
#
# Credentials are resolved at request time, in priority order:
#   1. auths/<file>.json type=vertex
#                            — Vertex AI OAuth from the management UI's
#                              "Vertex AI service account" upload.
#   2. cfg.VertexCompatAPIKey — `vertex-api-key:` entries in config.yaml.
#                              Requires base-url (third-party endpoints).
#   3. cfg.GeminiKey         — `gemini-api-key:` entries in config.yaml.
#                              (Generative Language API)
#   4. env GCP_SA_JSON_PATH  — real Vertex AI OAuth from disk
#                              (requires GCP_PROJECT_ID; GCP_REGION optional).
#   5. env GCP_API_KEY       — Generative Language API fallback.
#
# The binary boots with NO credentials configured; /v1/embeddings returns
# 503 with a clear message until creds appear. config.yaml AND the auth
# dir are mtime-watched, so uploads via the management UI take effect
# without restarting the container.
#
# Build (from the repository root, NOT from this directory):
#   docker build -f cmd/cliproxyapi-embed/Dockerfile -t cliproxyapi-embed:latest .
#
# Run (creds via config.yaml / management UI — recommended):
#   docker run -d --name cliproxyapi-embed \
#     -p 8317:8317 \
#     -v $PWD/config.yaml:/CLIProxyAPI/config.yaml \
#     -v $PWD/auths:/root/.cli-proxy-api \
#     -v $PWD/logs:/CLIProxyAPI/logs \
#     cliproxyapi-embed:latest
#
# Run (creds via env, Vertex OAuth):
#   docker run -d --name cliproxyapi-embed \
#     -p 8317:8317 \
#     -e GCP_SA_JSON_PATH=/secrets/vertex-sa.json \
#     -e GCP_PROJECT_ID=your-project \
#     -e GCP_REGION=us-central1 \
#     -v $PWD/config.yaml:/CLIProxyAPI/config.yaml \
#     -v $PWD/auths:/root/.cli-proxy-api \
#     -v $PWD/logs:/CLIProxyAPI/logs \
#     -v $PWD/vertex-sa.json:/secrets/vertex-sa.json:ro \
#     cliproxyapi-embed:latest

FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" \
    -o ./cliproxyapi-embed ./cmd/cliproxyapi-embed/

FROM alpine:3.23

RUN apk add --no-cache tzdata ca-certificates

RUN mkdir /CLIProxyAPI

COPY --from=builder /app/cliproxyapi-embed /CLIProxyAPI/cliproxyapi-embed
COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai
ENV EMBED_CONFIG_PATH=/CLIProxyAPI/config.yaml

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./cliproxyapi-embed"]
