# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS build
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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/rss-fulltext /usr/local/bin/rss-fulltext
COPY --from=build --chown=nonroot:nonroot /out/data /var/lib/rss-fulltext

EXPOSE 8080
USER nonroot:nonroot
VOLUME ["/var/lib/rss-fulltext"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/rss-fulltext", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/rss-fulltext"]
