FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /signalbot .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /signalbot /signalbot
USER nonroot
ENTRYPOINT ["/signalbot"]
