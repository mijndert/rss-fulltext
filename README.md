# rss-fulltext

Most RSS feeds only ship a headline and a one-paragraph teaser — to actually
read the piece you have to click out to the publisher's site, which usually
means a browser, an ad-loaded layout, and an internet connection.

`rss-fulltext` rewrites those feeds so the full article body travels with the
feed itself. For each entry it fetches the linked page, pulls the main content
out with readability, sanitises it, and writes the enriched feed to disk in
three formats: RSS (`<name>.xml`), Atom (`<name>.atom`), and JSON Feed
(`<name>.json`). Files are regenerated on a per-feed schedule and served as
static files over HTTP.

Useful when you want to:

- **Read offline.** Articles ship with the feed.
- **Read on an e-reader.** Full text instead of a link to a page your e-reader can't render.
- **Escape publisher chrome.** No popovers, ads, or newsletter modals.
- **Archive what you read.** Each refresh writes plain files to disk.

## Quick start

### One-line Docker

```sh
curl -O https://raw.githubusercontent.com/mijndert/rss-fulltext/main/feeds.yaml
docker run -d --name rss-fulltext \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/feeds.yaml:/etc/rss-fulltext/feeds.yaml:ro" \
  -v rss-fulltext-data:/var/lib/rss-fulltext \
  -e LISTEN_ADDR=:8080 \
  -e CONFIG_PATH=/etc/rss-fulltext/feeds.yaml \
  --read-only \
  --cap-drop=ALL \
  --security-opt=no-new-privileges:true \
  ghcr.io/mijndert/rss-fulltext:latest
```

The `--read-only`, `--cap-drop=ALL`, and `--security-opt=no-new-privileges`
flags match the hardening that Compose applies by default — the named volume
remains writable for the cache and generated feeds.

That's it. After a few seconds:

```sh
curl http://127.0.0.1:8080/feeds.json | jq
```

Endpoints:

- `http://127.0.0.1:8080/<name>.xml` — RSS 2.0
- `http://127.0.0.1:8080/<name>.atom` — Atom
- `http://127.0.0.1:8080/<name>.json` — JSON Feed
- `http://127.0.0.1:8080/feeds.json` — status of every configured feed
- `http://127.0.0.1:8080/metrics` — Prometheus metrics
- `http://127.0.0.1:8080/healthz` — liveness probe

### Docker Compose

If you'd rather use Compose (gets you the locked-down `read_only`, `cap_drop`,
`no-new-privileges` defaults out of the box):

```sh
git clone https://github.com/mijndert/rss-fulltext.git
cd rss-fulltext
docker compose up -d
```

Compose binds the published port to loopback (`127.0.0.1:8080`). To expose
externally, put a reverse proxy in front of it — see [Reverse proxy](#reverse-proxy)
below.

### Pre-built binary

Each tagged release ships linux/macOS/FreeBSD binaries for amd64 and arm64 on
the [Releases page](https://github.com/mijndert/rss-fulltext/releases). Verify
the tarball against `checksums.txt`, extract, and run:

```sh
CONFIG_PATH=./feeds.yaml OUTPUT_DIR=./out CACHE_DIR=./cache ./rss-fulltext
```

The binary also accepts a few subcommands:

```sh
rss-fulltext version       # print version, commit, build date
rss-fulltext healthcheck   # probe /healthz on the local port, exit 0 if healthy
```

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
# or for the one-line run above:
docker restart rss-fulltext
```

## Environment variables

All have defaults; override via `-e` on `docker run`, the `environment:` block
in Compose, or the process environment for the bare binary.

| Variable | Default | Notes |
| --- | --- | --- |
| `LISTEN_ADDR` | `127.0.0.1:8080` (binary), `:8080` (compose) | bare binary binds loopback only — set `:8080` to listen on all interfaces in containers |
| `CONFIG_PATH` | (required) | path to `feeds.yaml` |
| `OUTPUT_DIR` | `/var/lib/rss-fulltext/feeds` | where `<name>.xml`, `<name>.atom`, `<name>.json` files land |
| `CACHE_DIR` | `/var/lib/rss-fulltext/cache` | per-article cache; sha256 keys |
| `CACHE_TTL` | `24h` | how long a fetched article body stays cached; `0` disables caching |
| `JANITOR_INTERVAL` | `1h` | how often expired cache files are purged; `0` disables the janitor |
| `CONCURRENCY` | `4` | parallel article fetches per refresh |
| `REQUEST_TIMEOUT` | `20s` | per-outbound-HTTP-request |
| `READ_TIMEOUT` | `30s` | full request-read deadline on the HTTP server |
| `WRITE_TIMEOUT` | `30s` | response-write deadline on the HTTP server |
| `FEED_TIMEOUT` | `5m` | whole-refresh budget per feed |
| `MAX_ARTICLE_BYTES` | `5242880` | hard cap on body read by readability |
| `MAX_FEED_BYTES` | `4194304` | hard cap on the source-feed XML |
| `MAX_ITEMS_PER_FEED` | `50` | truncate longer feeds |
| `NEGATIVE_CACHE_TTL` | `1h` | how long extractor errors are cached |
| `MAX_STALENESS` | `24h` | how long to keep serving the previous file when upstream returns 0 items |
| `USER_AGENT` | `rss-fulltext/2.0` | sent on every outbound fetch |
| `ALLOW_PRIVATE_ADDRESSES` | `false` | set `true` only for local testing — disables the SSRF guard |

## Reverse proxy

Run rss-fulltext on loopback and put a reverse proxy in front for TLS and
hostname routing. Examples:

### Caddy

```
rss.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Caddy will obtain and renew a Let's Encrypt certificate automatically.

### nginx

```nginx
server {
    listen 443 ssl http2;
    server_name rss.example.com;

    ssl_certificate     /etc/letsencrypt/live/rss.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/rss.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

### Traefik (Docker)

Add labels to the `rss-fulltext` service in `docker-compose.yml`:

```yaml
labels:
  - "traefik.enable=true"
  - "traefik.http.routers.rss.rule=Host(`rss.example.com`)"
  - "traefik.http.routers.rss.entrypoints=websecure"
  - "traefik.http.routers.rss.tls.certresolver=letsencrypt"
  - "traefik.http.services.rss.loadbalancer.server.port=8080"
```

The container does not need `ports:` published when behind Traefik on the same
Docker network.

> **Note.** `/metrics` shares the same listener as the feed plane. If you
> expose the service publicly (e.g. `LISTEN_ADDR=:8080` plus a public reverse
> proxy), restrict `/metrics` at the proxy or only proxy `/<name>.{xml,atom,json}`
> and `/feeds.json`.

## Persistence

Compose creates a named volume `data` mounted at `/var/lib/rss-fulltext`. It
holds the generated feed files and the article cache. Inspect with:

```sh
docker compose exec rss-fulltext ls /var/lib/rss-fulltext/feeds
docker volume inspect rss-fulltext_data
```

Removing the volume (`docker compose down -v`) wipes both the cache and the
generated feeds. The feeds will be regenerated on next start.

## Pinning a version

`docker-compose.yml` uses `ghcr.io/mijndert/rss-fulltext:latest`. To pin:

```yaml
image: ghcr.io/mijndert/rss-fulltext:1.2.3
```

Tagged versions (`vX.Y.Z`) publish both Docker images and pre-built binaries
via GitHub Releases.

## Building locally

```sh
docker build -t rss-fulltext:dev .
# then edit docker-compose.yml to use rss-fulltext:dev
```

Or run the binary directly:

```sh
go build -o rss-fulltext ./
CONFIG_PATH=./feeds.yaml OUTPUT_DIR=./out CACHE_DIR=./cache ./rss-fulltext
```
