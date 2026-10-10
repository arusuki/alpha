FROM ubuntu:24.04 AS build
RUN apt-get update && apt-get install -y --no-install-recommends gcc make libc6-dev linux-libc-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY Makefile ./
COPY include/ include/
COPY src/ src/
RUN make -j BUILD=build build/dram-bwd

FROM ubuntu:24.04
COPY --from=build /src/build/dram-bwd /usr/local/bin/dram-bwd
RUN mkdir -p /run/dram-bw
ENTRYPOINT ["/usr/local/bin/dram-bwd"]
