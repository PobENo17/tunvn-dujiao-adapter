# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
# The adapter is standard-library only, so there is no go.sum to copy.
COPY go.mod ./
COPY *.go ./
COPY cmd ./cmd
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/tunvn-adapter .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 adapter \
    && mkdir -p /data && chown adapter:adapter /data
WORKDIR /data
COPY --from=build /out/tunvn-adapter /usr/local/bin/tunvn-adapter
USER adapter
ENV LISTEN_ADDR=0.0.0.0:8080 \
    DATA_FILE=/data/orders.json
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
    CMD wget -qO- http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/usr/local/bin/tunvn-adapter"]
