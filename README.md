# rss-feedgen

Takes summary-only RSS feeds, fetches every linked article, extracts the body
with readability, and writes the enriched feed to disk as `<name>.xml`. Files
are regenerated on a per-feed schedule and served as static XML over HTTP.

## Quick start

```sh
git clone https://github.com/mijndert/rss-feedgen.git
cd rss-feedgen
docker compose up -d
```

The default `feeds.yaml` ships with three example feeds. On first start the
service refreshes each one (staggered) and exposes:

- `http://127.0.0.1:8080/<name>.xml` — the enriched RSS for each feed
- `http://127.0.0.1:8080/feeds.json` — status of every configured feed
- `http://127.0.0.1:8080/healthz` — liveness probe

Compose binds to loopback (`127.0.0.1:8080`). To expose externally, put a
reverse proxy in front (Caddy, nginx, Traefik) and terminate TLS there.

## Configuration

`feeds.yaml` is the source of truth.

```yaml
default_interval: 1h        # used when a feed omits `interval`

feeds:
  - name: techcrunch        # appears in /<name>.xml — must match [a-z0-9_-]+
    url: https://techcrunch.com/feed/
    title: TechCrunch (full text)
    interval: 30m           # Go duration; minimum 1m

  - name: hn
    url: https://hnrss.org/frontpage
    title: Hacker News (full text)
```

Edit `feeds.yaml` and restart the container to pick up changes:

```sh
docker compose restart
```

## Environment variables

All have defaults in `docker-compose.yml`. Override there or with `.env`.

| Variable | Default | Notes |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8080` | inside the container |
| `CONFIG_PATH` | `/etc/rss-feedgen/feeds.yaml` | mounted from `./feeds.yaml` |
| `OUTPUT_DIR` | `/var/lib/rss-feedgen/feeds` | where `<name>.xml` files land |
| `CACHE_DIR` | `/var/lib/rss-feedgen/cache` | per-article cache; sha256 keys |
| `CACHE_TTL` | `24h` | how long a fetched article body stays cached |
| `JANITOR_INTERVAL` | `1h` | how often expired cache files are purged |
| `CONCURRENCY` | `4` | parallel article fetches per refresh |
| `REQUEST_TIMEOUT` | `20s` | per-outbound-HTTP-request |
| `FEED_TIMEOUT` | `5m` | whole-refresh budget per feed |
| `MAX_ARTICLE_BYTES` | `5242880` | hard cap on body read by readability |
| `MAX_FEED_BYTES` | `4194304` | hard cap on the source-feed XML |
| `MAX_ITEMS_PER_FEED` | `50` | truncate longer feeds |
| `USER_AGENT` | `rss-feedgen/2.0` | sent on every outbound fetch |
| `ALLOW_PRIVATE_ADDRESSES` | `false` | set `true` only for local testing — disables the SSRF guard |

## Persistence

Compose creates a named volume `data` mounted at `/var/lib/rss-feedgen`. It
holds the generated `.xml` files and the article cache. Inspect with:

```sh
docker compose exec rss-feedgen ls /var/lib/rss-feedgen/feeds
docker volume inspect rss-feedgen_data
```

Removing the volume (`docker compose down -v`) wipes both the cache and the
generated feeds. The feeds will be regenerated on next start.

## Pinning a version

`docker-compose.yml` uses `ghcr.io/mijndert/rss-feedgen:latest`. To pin:

```yaml
image: ghcr.io/mijndert/rss-feedgen:1.2.3
```

Tagged versions are published by the `docker` workflow when a `vX.Y.Z` git tag
is pushed.

## Building locally

```sh
docker build -t rss-feedgen:dev .
# then edit docker-compose.yml to use rss-feedgen:dev
```

Or run the binary directly:

```sh
go build -o rss-feedgen ./
CONFIG_PATH=./feeds.yaml OUTPUT_DIR=./out CACHE_DIR=./cache ./rss-feedgen
```
