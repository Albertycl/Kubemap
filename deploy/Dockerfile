# Kubemap image: the server (kubemap-portal) and the node agent (kubemap-agent)
# on distroless, non-root. The server Deployment sets its own command; the
# node agent DaemonSet uses the image entrypoint.
#
# Build the web UI first (make web): web/dist is embedded in the server.
# "make image" does both.
#
# Both base images are pinned by digest (the multi-arch index), so a rebuild
# uses exactly the same bases. To move to a newer base, read its digest
# without pulling it:
#   docker buildx imagetools inspect golang:1.27.1
#   docker buildx imagetools inspect gcr.io/distroless/static:nonroot
# and replace the digest below in the same commit as the reason.
FROM golang:1.27.1@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETARCH=amd64
# Two compile jobs at most: the build VM is short on memory (client-go).
RUN for c in kubemap-portal kubemap-agent; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} GOFLAGS=-p=2 go build -trimpath \
      -ldflags "-s -w -X kubemap/pkg/version.Version=${VERSION}" \
      -o /out/$c ./cmd/$c || exit 1; \
    done

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG VERSION=dev
LABEL org.opencontainers.image.title="kubemap" \
      org.opencontainers.image.description="Kubemap server and node agent" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/kubemap-portal /usr/local/bin/kubemap-portal
COPY --from=build /out/kubemap-agent /usr/local/bin/kubemap-agent
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kubemap-agent"]
