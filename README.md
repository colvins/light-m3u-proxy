# light-m3u-proxy

Lightweight IPTV HLS proxy written in Go. It rewrites M3U playlists so players fetch HLS playlists and media segments through this proxy.

## Features

- Serves `GET /iptv.m3u` from a local `channels.m3u` file.
- Rewrites channel URLs to `{PUBLIC_BASE_URL}/proxy?url=...`.
- Proxies HLS playlists and media segments through `GET /proxy?url=<encoded-url>`.
- Supports `GET`, `HEAD`, and `OPTIONS`.
- Uses `GET` upstream even when the client sends `HEAD`, then returns headers only.
- Follows upstream HTTP redirects in the server, up to 5 redirects.
- Rewrites non-comment playlist URI lines to proxy URLs.
- Flattens a single-variant HLS playlist by one level when possible.
- Streams binary media responses without writing them to disk.

## Quick Start

Create local config files from the public examples:

```bash
cp channels.example.m3u channels.m3u
cp docker-compose.sample.yml docker-compose.yml
```

Edit `channels.m3u` and add your own channel playlist URLs.

Edit `docker-compose.yml` and set `PUBLIC_BASE_URL` to the URL that players will use to reach this service:

```yaml
environment:
  PUBLIC_BASE_URL: "http://example.com:5050"
  PORT: "8080"
```

Start the service:

```bash
docker compose up -d --build
```

Open the generated playlist in your player:

```text
http://example.com:5050/iptv.m3u
```

## Example Channels File

```m3u
#EXTM3U
#EXTINF:-1 group-title="Example",Example Channel
http://example.com/live/example.m3u8
```

## Test Commands

```bash
curl -sL http://127.0.0.1:5050/iptv.m3u | head -40

URL=$(curl -sL http://127.0.0.1:5050/iptv.m3u | awk '/^http/ {print; exit}')
echo "$URL"

curl -sL "$URL" | head -80
```

## Notes

- `channels.m3u` and `docker-compose.yml` are local runtime files and are intentionally ignored by Git.
- Do not commit real IPTV source URLs, private domains, credentials, certificates, or local network details.
