#!/bin/bash
DIR="$(cd "$(dirname "$0")" && pwd)"
docker run --rm -p 8081:80 \
  -v "$DIR":/video:ro \
  -v "$DIR/nginx.conf":/etc/nginx/conf.d/default.conf:ro \
  nginx:alpine
