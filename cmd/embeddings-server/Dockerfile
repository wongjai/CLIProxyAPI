# Dockerfile for the embeddings-server variant of CLIProxyAPI.
#
# This image bundles the standard cliproxy SDK runtime PLUS an additional
# POST /v1/embeddings route backed by Google Vertex AI. It is a drop-in
# replacement for the upstream `eceasy/cli-proxy-api` image when you also
# want OpenAI-compatible embeddings.
#
# Build (from the repository root, NOT from this directory):
#   docker build -f cmd/embeddings-server/Dockerfile -t embeddings-server:latest .
#
# Run:
#   docker run -d --name embeddings-server \
#     -p 8317:8317 \
#     -e GCP_SA_JSON_PATH=/secrets/vertex-sa.json \
#     -e GCP_PROJECT_ID=your-project \
#     -e GCP_REGION=us-central1 \
#     -v $PWD/config.yaml:/CLIProxyAPI/config.yaml \
#     -v $PWD/auths:/root/.cli-proxy-api \
#     -v $PWD/logs:/CLIProxyAPI/logs \
#     -v $PWD/vertex-sa.json:/secrets/vertex-sa.json:ro \
#     embeddings-server:latest

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
    -o ./embeddings-server ./cmd/embeddings-server/

FROM alpine:3.23

RUN apk add --no-cache tzdata ca-certificates

RUN mkdir /CLIProxyAPI

COPY --from=builder /app/embeddings-server /CLIProxyAPI/embeddings-server
COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai
ENV EMBED_CONFIG_PATH=/CLIProxyAPI/config.yaml

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./embeddings-server"]
