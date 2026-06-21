#!/bin/sh

set -e

find . -name '*.MP4' -type f | sort | awk '{printf "file \047%s\047\n", $0}' > mylist.txt
ffmpeg -f concat -safe 0 -i mylist.txt -c:s copy -c:v libx265 -filter:v "setpts=PTS/3,crop=in_w:in_h-262:0:out_h" -crf 28 -map 0:0 "${PWD##*/}.mp4"
rm mylist.txt
