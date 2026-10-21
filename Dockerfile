FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/eh-blacklist-server ./cmd/server

FROM alpine:3.20
WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/eh-blacklist-server /app/eh-blacklist-server

EXPOSE 8787

ENV ADDR=:8787
ENV DB_PATH=/data/blacklist.db

VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8787/health >/dev/null || exit 1

ENTRYPOINT ["/app/eh-blacklist-server"]
