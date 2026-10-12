FROM golang:1.26 AS manager
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/rootless-docker/ cmd/rootless-docker/
COPY internal/rootless/ internal/rootless/
COPY internal/containerorigin/ internal/containerorigin/
COPY internal/procfs/ internal/procfs/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /rootless-docker ./cmd/rootless-docker

FROM ubuntu:24.04 AS build
RUN apt-get update && apt-get install -y --no-install-recommends gcc make libc6-dev linux-libc-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY ctools/dram-bw/Makefile ./
COPY ctools/dram-bw/include/ include/
COPY ctools/dram-bw/src/ src/
RUN make -j BUILD=build LDFLAGS=-static build/dram-bwd

FROM scratch
COPY --from=build /src/build/dram-bwd /usr/local/bin/dram-bwd
COPY --from=manager /rootless-docker /rootless-docker
WORKDIR /run/dram-bw
LABEL project-alpha.dram-settings="2"
ENTRYPOINT ["/rootless-docker", "--dram-daemon"]
