# Both images are pinned by digest, so a build uses exactly these bytes even
# if a tag is moved. To update one, look up the new digest, for example:
#   docker buildx imagetools inspect golang:1.25.14-bookworm
#   docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot
FROM golang:1.25.14-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437 AS build
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
COPY internal ./internal
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /signalbot .
# The bot's state directory, owned by the nonroot user. There is no shell in
# the final image to make it, so it is made here and copied.
RUN mkdir /data

# distroless/static: no shell or package manager, CA certificates for TLS to
# the model's API, and the unprivileged nonroot user. The binary stays root-owned, so the
# bot cannot modify it.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /signalbot /signalbot
COPY --from=build --chown=65532:65532 /data /data
ENV STATE_DIRECTORY=/data
# Numeric, so Kubernetes' runAsNonRoot can verify it.
USER 65532:65532
ENTRYPOINT ["/signalbot"]
