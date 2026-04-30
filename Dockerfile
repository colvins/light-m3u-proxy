FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/light-m3u-proxy .

FROM alpine:3.20

WORKDIR /app
COPY --from=build /out/light-m3u-proxy /usr/local/bin/light-m3u-proxy
COPY channels.m3u /app/channels.m3u

EXPOSE 5050
ENTRYPOINT ["/usr/local/bin/light-m3u-proxy"]
