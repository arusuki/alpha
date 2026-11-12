ARG ENGINE_IMAGE=docker:28-dind
FROM ${ENGINE_IMAGE} AS engine
FROM python:3.12-slim-bookworm
COPY --from=engine /usr/local/bin/docker /usr/local/bin/dockerd /usr/local/bin/runc /usr/local/bin/containerd /usr/local/bin/containerd-shim-runc-v2 /usr/local/bin/docker-init /usr/local/bin/docker-proxy /usr/bin/
RUN sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources && apt-get update && apt-get install -y --no-install-recommends libseccomp2 libsystemd0 iproute2 iptables iputils-ping netcat-openbsd sudo util-linux && rm -rf /var/lib/apt/lists/*
