FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -o /out/eh-blacklist-server ./cmd/server

FROM alpine:3.20
WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/eh-blacklist-server /app/eh-blacklist-server

EXPOSE 8787

ENV ADDR=:8787
ENV DB_PATH=/data/blacklist.db

VOLUME ["/data"]

ENTRYPOINT ["/app/eh-blacklist-server"]