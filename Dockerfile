# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine@sha256:8d22e29d960bc50cd025d93d5b7c7d220b1ee9aa7a239b3c8f55a57e987e8d45 AS build
WORKDIR /src

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOFLAGS="-trimpath"

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY internal ./internal

RUN go build -ldflags="-s -w" -o /out/rss-fulltext ./

RUN mkdir -p /out/data/feeds /out/data/cache && \
    chown -R 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639
COPY --from=build /out/rss-fulltext /usr/local/bin/rss-fulltext
COPY --from=build --chown=nonroot:nonroot /out/data /var/lib/rss-fulltext

EXPOSE 8080
USER nonroot:nonroot
VOLUME ["/var/lib/rss-fulltext"]
ENTRYPOINT ["/usr/local/bin/rss-fulltext"]
