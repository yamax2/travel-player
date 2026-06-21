#!/bin/bash
set -e

ffmpeg -f concat -safe 0 -i <(cat <<'EOF'
file '/Volumes/NO NAME/Normal/Front/NO20210101-000019-000001F.MP4'
file '/Volumes/NO NAME/Normal/Front/NO20260215-144120-000002F.MP4'
file '/Volumes/NO NAME/Normal/Front/NO20260215-144420-000003F.MP4'
EOF
) -filter:v "setpts=PTS/3,crop=in_w:in_h-251:0:out_h" -c:v libsvtav1 -crf 34 -preset 4 -an "03.mp4"
