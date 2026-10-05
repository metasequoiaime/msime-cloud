FROM node:24-alpine AS admin-build
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
FROM rust:1.97.1-bookworm@sha256:0e2bcaef56d041a486784e54104a81aebe0da44bd03019bd70bc0401e42e4a97 AS native-build
WORKDIR /src/third_party/msime
COPY third_party/msime ./
RUN cargo build -p msime-backend-engine --release --locked && install -D target/release/msime-backend-engine /build/msime-engine
# The dictionary release the clients pin, verified against the submodule's lock file, and the submodule's helpcode tables.
FROM debian:bookworm-slim AS resources
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
