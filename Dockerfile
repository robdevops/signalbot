# Both images are pinned by digest, so a build uses exactly these bytes even
# if a tag is moved. To update one, look up the new digest, for example:
#   docker buildx imagetools inspect golang:1.24.7-bookworm
#   docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot
FROM golang:1.24.7-bookworm@sha256:b8bae5bd9ba9b1f89b635c91c24cc75cea335a16fb5076310f38566fc674b1ec AS build
ARG VERSION=dev
# Never download a toolchain: build with the one in this image.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /src
# The one dependency (golang.org/x/image, for chart text) is downloaded once
# and checked against go.sum; after that, the build is offline.
COPY go.mod go.sum ./
RUN go mod download
ENV GOPROXY=off
COPY *.go ./
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /signalbot .

# distroless/static: no shell or package manager, CA certificates for TLS to
# OpenRouter, and the unprivileged nonroot user. The binary stays root-owned, so the
# bot cannot modify it.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /signalbot /signalbot
# Numeric, so Kubernetes' runAsNonRoot can verify it.
USER 65532:65532
ENTRYPOINT ["/signalbot"]
