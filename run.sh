#!/bin/sh

docker run -d --rm -p 8081:80 \
  -v "$PWD":/video:ro \
  -v "$PWD/nginx.conf":/etc/nginx/conf.d/default.conf:ro \
  --name travel_movies \
  nginx:alpine
