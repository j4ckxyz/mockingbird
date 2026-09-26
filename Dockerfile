# syntax=docker/dockerfile:1
#
# Multi-arch build: docker buildx build --platform linux/arm64,linux/amd64 .
# The Go build cross-compiles on the build host; the runtime image is
# distroless (CA certificates, no shell) and runs as a non-root user.

FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/mockingbird ./cmd/mockingbird
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/mockingbird /mockingbird
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV MB_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080 8443
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/mockingbird", "healthcheck"]
ENTRYPOINT ["/mockingbird"]
