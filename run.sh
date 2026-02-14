#!/bin/bash
docker run --rm -p 8081:80 \
  -v "$(cd "$(dirname "$0")/01" && pwd)":/video/01:ro \
  -v "$(cd "$(dirname "$0")" && pwd)/nginx.conf":/etc/nginx/conf.d/default.conf:ro \
  nginx:alpine
