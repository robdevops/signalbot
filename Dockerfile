FROM golang:1.24 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /signalbot .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /signalbot /signalbot
USER nonroot
ENTRYPOINT ["/signalbot"]
