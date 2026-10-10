FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
ARG GOPROXY=https://proxy.golang.org,direct
RUN go mod download golang.org/x/sys
COPY cmd/rootless-docker ./cmd/rootless-docker
COPY internal/rootless ./internal/rootless
RUN CGO_ENABLED=0 go build -trimpath -o /out/rootless-docker ./cmd/rootless-docker

# The daemon enters the host namespaces and uses the host's installed Docker,
# RootlessKit and systemd. Only the statically linked manager is shipped here.
FROM scratch
COPY --from=build /out/rootless-docker /rootless-docker
ENTRYPOINT ["/rootless-docker"]
CMD ["daemon", "--host-namespaces"]
