# 70mai GPS Tool

Converts 70mai dashcam SD card data (GPS log + video segments) into a single GPX file, a single ffmpeg encode script, and an `index.json` for the travels video player.

## Usage

```
go run tools/70mai-gps/main.go [-tz +05:00] [-input /Volumes/NO\ NAME] [-output 03]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-input` | `/Volumes/NO NAME` | Path to dashcam SD card |
| `-output` | `.` | Directory for output files (also used as base name) |
| `-tz` | `+05:00` | Local timezone offset, for converting video filename timestamps to UTC |

## SD Card Layout

The tool expects this structure on the SD card:

```
<input>/
  GPSData000001.txt      # GPS log (may be multiple GPSData*.txt)
  Normal/Front/
    NO20260215-144120-000001F.MP4
    NO20260215-144420-000002F.MP4
    ...
```

## Input Formats

### GPS file (`GPSData*.txt`)

```
$V02                                           <- header line (skipped)
1771119528,A,41.328448,69.333238,0,0,...       <- data lines
```

Fields: `raw_ts, status, lat, lon, heading_centideg, speed_cm_s, ax, ay, az, 0, 0, 0, 0`

- **Timestamp**: `utc_epoch = raw_ts + 8 * 3600` (dashcam stores CST-biased epoch)
- **Status**: `A` = active fix, `V` = void (skipped)
- **Lat/Lon**: decimal degrees (already standard, no NMEA DDMM conversion)
- **Heading**: divide by 100 -> degrees
- **Speed**: multiply by 0.036 -> km/h
- Records with `lat == 0 && lon == 0` are skipped

### Video filenames

Pattern: `NO20260215-144120-000002F.MP4` -> local time `2026-02-15 14:41:20`, sequence `002`

- Video 001 always has a wrong date (pre-GPS-lock default like `20210101`); it's kept in the output but gets no GPS
- Local time is converted to UTC: `utc = local_time - tz_offset`
- Duration is read via `ffprobe`

## Algorithm

1. **Parse GPS**: Read all `GPSData*.txt`, skip headers/void/zero-coords, apply +8h UTC correction, sort by timestamp
2. **Parse videos**: Glob `*.MP4` from `Normal/Front/`, extract time + sequence from filename, get duration via ffprobe, sort by sequence
3. **Group into trips**: Walk sorted videos; gaps >= 10 min start a new trip. Gaps > 24h are treated as implausible timestamps and don't trigger a trip break
4. **Collect GPS per trip**: For each video, binary-search GPS array for matching time range. Track `gpsOffset` = scaled time from trip start to first GPS fix in that trip
5. **Write outputs**: Single GPX (gap-filled to 1 point/sec), single ffmpeg script, single index.json

## Output

All files are written to the `-output` directory. The base name of that directory is used for file naming (e.g. `-output 03` produces `03.gpx`, `03.sh`, `03.mp4`).

### `<name>.gpx` — single GPX track

All GPS data across all trips, gap-filled to one trackpoint per second. Gaps between trips are filled by holding the last known position.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<gpx version="1.1" xmlns="http://www.topografix.com/GPX/1/1">
  <trk>
    <name>03</name>
    <trkseg>
      <trkpt lat="41.3272400" lon="69.3326440">
        <time>2026-02-15T09:41:20Z</time>
        <speed>14.0</speed>
        <course>185.0</course>
      </trkpt>
      ...
    </trkseg>
  </trk>
</gpx>
```

### `<name>.sh` — ffmpeg encode script

Concatenates all source MP4s from the SD card into a single 3x timelapse video using AV1.

```bash
#!/bin/bash
set -e

ffmpeg -f concat -safe 0 -i <(cat <<'EOF'
file '/Volumes/NO NAME/Normal/Front/NO20210101-000019-000001F.MP4'
file '/Volumes/NO NAME/Normal/Front/NO20260215-144120-000002F.MP4'
...
EOF
) -filter:v "setpts=PTS/3" -c:v libsvtav1 -crf 30 -preset 6 -pix_fmt yuv420p10le -an "03.mp4"
```

Notes:
- `libsvtav1` — AV1 encoder, better browser compatibility than H.265
- `-crf 30` — quality roughly equivalent to H.265 CRF 28
- `-preset 6` — balanced speed/quality
- `setpts=PTS/3` — 3x timelapse
- `-an` — drops audio (meaningless at 3x)
- Uses bash process substitution `<(...)` — requires `#!/bin/bash`, not `#!/bin/sh`
- Source paths point to SD card; card must be mounted when running

### `index.json` — player metadata

Each trip becomes a chapter. Chapter titles are the UTC start time of the first real video in the trip.

```json
{
  "title": "2026-02-15",
  "video": "03.mp4",
  "speed": 3,
  "tz": "+05:00",
  "gpx": "03.gpx",
  "chapters": [
    { "title": "09:41", "start": 0, "end": 1859.1, "gpsOffset": 60 },
    { "title": "11:32", "start": 1859.1, "end": 3053.8, "gpsOffset": 28 },
    { "title": "13:46", "start": 3053.8, "end": 4574.2, "gpsOffset": 27 }
  ]
}
```

- `start`/`end` — seconds in the combined (3x timelapse) video
- `gpsOffset` — seconds from chapter start to first GPS fix (scaled to video time). Present only for chapters with GPS coverage
- Chapters without any GPS data omit `gpsOffset`

## Quirks & Edge Cases

- **Pre-GPS-lock video dates**: If the dashcam hasn't been used for a while, the first video(s) may have a default date (e.g. `2021-01-01`) recorded before GPS lock. The tool detects these by comparing video timestamps against the GPS data range — if a video's date is >24h away from any GPS data, it's considered implausible. Such videos are kept in the same trip as their neighbors (not split into a separate trip) and their dates are not used for titles
- **macOS `._` files**: Resource fork files on the SD card are filtered out
- **GPS gaps**: Some videos may have no GPS coverage (dashcam lost signal). These videos are included in the combined video but have no GPS trackpoints
- **gpsOffset on chapters**: Represents the scaled time (in video seconds) from the start of the chapter to when GPS data first becomes available
- **Trip detection**: Gaps >= 10 min between consecutive videos start a new trip. Gaps > 24h are treated as bad timestamps and ignored for trip splitting
- **ffprobe fallback**: If ffprobe fails for a video, duration defaults to 180s (3 min)
