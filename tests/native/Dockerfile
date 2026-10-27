FROM ubuntu:25.10
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates cmake g++ make pkg-config libboost-json-dev libfmt-dev libspdlog-dev \
    libsqlite3-dev libcurl4-openssl-dev nlohmann-json3-dev libtomlplusplus-dev \
    && rm -rf /var/lib/apt/lists/*
