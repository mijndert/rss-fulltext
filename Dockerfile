# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine@sha256:91eda9776261207ea25fd06b5b7fed8d397dd2c0a283e77f2ab6e91bfa71079d AS build
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

FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1
COPY --from=build /out/rss-fulltext /usr/local/bin/rss-fulltext
COPY --from=build --chown=nonroot:nonroot /out/data /var/lib/rss-fulltext

EXPOSE 8080
USER nonroot:nonroot
VOLUME ["/var/lib/rss-fulltext"]
ENTRYPOINT ["/usr/local/bin/rss-fulltext"]
