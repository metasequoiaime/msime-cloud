# 管理后台只产出静态文件，与目标架构无关，所以在构建机自己的架构上构建一次，不为每个目标架构在模拟器里重跑。
FROM --platform=$BUILDPLATFORM node:24-alpine AS admin-build
WORKDIR /src
RUN npm install --global pnpm@10.15.0
COPY admin-web/package.json admin-web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY admin-web/ ./
RUN pnpm build

FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY VERSION version.go ./
COPY cmd ./cmd
COPY internal ./internal
COPY admin-web/embed.go ./admin-web/embed.go
COPY --from=admin-build /src/dist ./admin-web/dist
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /msime-server ./cmd/msime-server
# The query process msime-cloud runs per request: msime-backend-engine from the msime submodule, the same Rust engine the clients ship. The toolchain is the submodule's rust-toolchain.toml; the image is the one msime's own Linux build gate uses.
# 引擎在构建机自己的架构上交叉编译到目标架构：在 QEMU 里给 linux/arm64 跑 rustc，单这一步就让发布镜像的 job 超过 30 分钟上限被取消。ring 和内置的 SQLite 要编译 C 代码，所以目标架构不同时还要装 Debian 的交叉 gcc，它链接的 bookworm glibc 与 distroless 运行镜像一致。
FROM --platform=$BUILDPLATFORM rust:1.97.1-bookworm@sha256:0e2bcaef56d041a486784e54104a81aebe0da44bd03019bd70bc0401e42e4a97 AS native-build
ARG BUILDARCH
ARG TARGETARCH
WORKDIR /src/third_party/msime
COPY third_party/msime ./
RUN set -eu; \
    case "${TARGETARCH:-amd64}" in \
      amd64) triple=x86_64-unknown-linux-gnu gnu=x86_64-linux-gnu packages="gcc-x86-64-linux-gnu libc6-dev-amd64-cross" ;; \
      arm64) triple=aarch64-unknown-linux-gnu gnu=aarch64-linux-gnu packages="gcc-aarch64-linux-gnu libc6-dev-arm64-cross" ;; \
      *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    if [ "${TARGETARCH:-amd64}" != "${BUILDARCH:-amd64}" ]; then \
      apt-get update && apt-get install -y --no-install-recommends $packages && rm -rf /var/lib/apt/lists/*; \
      rustup target add "$triple"; \
      export "CARGO_TARGET_$(echo "$triple" | tr 'a-z-' 'A-Z_')_LINKER=$gnu-gcc" "CC_$(echo "$triple" | tr '-' '_')=$gnu-gcc" "AR_$(echo "$triple" | tr '-' '_')=$gnu-ar"; \
    fi; \
    cargo build -p msime-backend-engine --release --locked --target "$triple"; \
    install -D "target/$triple/release/msime-backend-engine" /build/msime-engine
# The dictionary release the clients pin, verified against the submodule's lock file, and the submodule's helpcode tables.
# 资源是数据文件，与架构无关，这一阶段同样在构建机自己的架构上运行。
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS resources
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates python3 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY scripts/fetch_engine_resources.py ./scripts/fetch_engine_resources.py
COPY third_party/msime/resources/desktop-dictionary.lock.json ./third_party/msime/resources/desktop-dictionary.lock.json
COPY third_party/msime/resources/helpcodes ./third_party/msime/resources/helpcodes
RUN python3 scripts/fetch_engine_resources.py /resources
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=native-build /build/msime-engine /usr/local/bin/msime-engine
COPY --from=resources --chown=65532:65532 /resources /usr/share/msime
COPY third_party/msime/LICENSE /licenses/msime-LICENSE
COPY third_party/msime/resources/licenses/msime-engine-dictionary-NOTICE.md /licenses/msime-dictionary-NOTICE.md
COPY --from=build /msime-server /msime-server
COPY THIRD_PARTY_NOTICES.txt /licenses/MSIME-Server-THIRD-PARTY.txt
EXPOSE 8080
ENTRYPOINT ["/msime-server", "-config", "/config/config.json"]
