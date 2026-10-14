# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.25.3-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY main.go admin.go ./

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/status .

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata \
    && mkdir -p /data /opt/status

COPY --from=build /out/status /usr/local/bin/status
COPY status.html /opt/status/status.html
COPY admin.html /opt/status/admin.html
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod 755 /usr/local/bin/docker-entrypoint.sh /usr/local/bin/status

WORKDIR /data
EXPOSE 8080
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/api/status || exit 1

ENTRYPOINT ["docker-entrypoint.sh"]
