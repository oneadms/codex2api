# syntax=docker/dockerfile:1

# ============================================================
# Stage 1: 构建前端 (React + Vite)
# 前端产物是纯静态文件，只需构建一次，与目标平台无关
# ============================================================
FROM --platform=$BUILDPLATFORM node:20-alpine AS frontend-builder

ARG BUILD_VERSION=dev

WORKDIR /frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund
COPY frontend/ .
RUN VITE_APP_VERSION=${BUILD_VERSION} npm run build

# ============================================================
# Stage 2: 构建 Go 后端
# 使用 BUILDPLATFORM 原生运行 + TARGETARCH 交叉编译
# ============================================================
FROM --platform=$BUILDPLATFORM golang:1.26.9-alpine AS go-builder

ARG TARGETARCH
ARG BUILD_VERSION=dev

# 国内构建走 goproxy.cn，避免直连 proxy.golang.org 断流（unexpected EOF）
ENV GOPROXY=https://goproxy.cn,direct

WORKDIR /app
COPY go.mod go.sum ./
# Keep the module cache in the image layer: the diagnostic worker uses these
# exact dependencies offline after deployment.
RUN go mod download

COPY . .
COPY --from=frontend-builder /frontend/dist ./frontend/dist

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w -X github.com/codex2api/internal/version.Version=${BUILD_VERSION}" -o /codex2api .

# This stage follows TARGETPLATFORM. The cross-build stage's Go executable
# follows BUILDPLATFORM and would not run in an arm64 runtime image.
FROM golang:1.26.9-alpine AS diagnostic-toolchain

# ============================================================
# Stage 3: 最终运行镜像
# ============================================================
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata git github-cli nodejs npm

COPY --from=diagnostic-toolchain /usr/local/go /usr/local/go
COPY --from=go-builder /go/pkg/mod /opt/codex2api/go-mod
# Login shells may replace PATH, so expose Go through the standard bin path too.
RUN ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt
ENV PATH="/usr/local/go/bin:${PATH}" \
    GOMODCACHE=/opt/codex2api/go-mod \
    GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local CGO_ENABLED=0

# Verify the native compiler and cached modules in BOTH runtime architectures,
# with networking disabled. This catches missing Go and missing dependencies.
COPY go.mod go.sum /tmp/diag-go-smoke/
RUN --network=none cd /tmp/diag-go-smoke \
    && go version \
    && go test -mod=readonly -count=1 -run '^TestParseAny$' github.com/tidwall/gjson \
    && rm -rf /tmp/diag-go-smoke

COPY --from=go-builder /codex2api /usr/local/bin/codex2api
COPY internal/diag/codex_runner.mjs /opt/codex2api/codex-sdk/codex_runner.mjs
COPY internal/diag/package.json internal/diag/package-lock.json /opt/codex2api/codex-sdk/
RUN npm ci --omit=dev --no-audit --no-fund --prefix /opt/codex2api/codex-sdk

# 内核、订阅及节点状态与现有 /data 卷一同持久化。
ENV DATA_DIR=/data
COPY third_party/sub2api /usr/share/codex2api/licenses/sub2api

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/codex2api"]
