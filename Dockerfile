# check=error=true
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:1fb7391fd54a953f15205f2cfe71ba48ad358c381d4e2efcd820bfca921cd6c6 AS builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY main.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /plex-exporter main.go

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e3f945647ffb95b5839c07038d64f9811adf17308b9121d8a2b87b6a22a80a39

COPY --from=builder /plex-exporter /plex-exporter
USER nonroot:nonroot
ENTRYPOINT ["/plex-exporter"]
