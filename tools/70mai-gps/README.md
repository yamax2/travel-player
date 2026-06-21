# 70mai GPS Tool

Converts 70mai dashcam SD card data (GPS log + video segments) into a single GPX file, a single ffmpeg encode script, and an `index.json` for the travels video player.

## Usage

```
go run tools/70mai-gps/main.go [-tz +05:00] [-input /Volumes/NO\ NAME] [-output 03] [-skew 0]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-input` | `/Volumes/NO NAME` | Path to dashcam SD card |
| `-output` | `.` | Directory for output files (also used as base name) |
| `-tz` | `+05:00` | Local timezone offset, for converting video filename timestamps to UTC |
| `-skew` | `0` | Seconds the GPS clock runs **ahead** of the video clock (RTC). Subtracted from GPS timestamps so the track lines up with the footage. See [GPS vs video clock skew](#gps-vs-video-clock-skew) |

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

1. **Parse GPS**: Read all `GPSData*.txt`, skip headers/void/zero-coords, apply +8h UTC correction, subtract `-skew` (GPS-vs-RTC clock skew), sort by timestamp
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
  "mapZoom": 15,
  "trackColor": "#4488cc",
  "chapters": [
    { "title": "09:41", "start": 0, "end": 1859.1, "gpsOffset": 60, "gpsStart": "2026-02-15T09:41:20Z" },
    { "title": "11:32", "start": 1859.1, "end": 3053.8, "gpsOffset": 28, "gpsStart": "2026-02-15T11:32:48Z" },
    { "title": "13:46", "start": 3053.8, "end": 4574.2, "gpsOffset": 27, "gpsStart": "2026-02-15T13:46:05Z" }
  ]
}
```

- `mapZoom` — default Leaflet zoom level for the map (player default `15` if absent)
- `trackColor` — track polyline color on the map (player default `#4488cc` if absent)
- `start`/`end` — seconds in the combined (3x timelapse) video
- `gpsOffset` — seconds from chapter start to first GPS fix (scaled to video time). Present only for chapters with GPS coverage
- `gpsStart` — absolute UTC time of the chapter's first GPS fix. The player uses it to slice the single GPX per chapter and to map video time → GPS time. Present only for chapters with GPS coverage
- `gaps` — recording pauses inside the chapter (camera stopped writing, then resumed within the same trip). Each entry is `{ "t": <absolute video-second of the cut>, "d": <real seconds skipped> }`. The concatenated video omits the pause, so the player adds `d` back to the video→GPS time mapping past `t`, keeping the marker in sync after the stop. Only pauses after the GPS fix are recorded (pre-fix pauses are absorbed into `gpsOffset`). Omitted when there are none
- Chapters without any GPS data omit `gpsOffset`/`gpsStart`/`gaps`

## Quirks & Edge Cases

- **Pre-GPS-lock video dates**: If the dashcam hasn't been used for a while, the first video(s) may have a default date (e.g. `2021-01-01`) recorded before GPS lock. The tool detects these by comparing video timestamps against the GPS data range — if a video's date is >24h away from any GPS data, it's considered implausible. Such videos are kept in the same trip as their neighbors (not split into a separate trip) and their dates are not used for titles
- **macOS `._` files**: Resource fork files on the SD card are filtered out
- **GPS gaps**: Some videos may have no GPS coverage (dashcam lost signal). These videos are included in the combined video but have no GPS trackpoints
- **gpsOffset on chapters**: Represents the scaled time (in video seconds) from the start of the chapter to when GPS data first becomes available
- **Trip detection**: Gaps >= 10 min between consecutive videos start a new trip. Gaps > 24h are treated as bad timestamps and ignored for trip splitting
- **ffprobe fallback**: If ffprobe fails for a video, duration defaults to 180s (3 min)

## GPS vs video clock skew

On some 70mai units the GPS chip's clock runs a fixed amount **ahead** of the camera's internal clock (RTC) — the RTC drives both the video file names and the burned-in on-screen timestamp, which is what the tool uses to line GPS up with the footage. When they disagree, the map marker is shifted by that skew (e.g. the marker shows the car still parked while the footage is already driving). One observed unit ran **~115 s ahead**.

Correct it with `-skew <seconds>` (GPS ahead of video → positive). The value is subtracted from every GPS timestamp, so the track and the per-chapter offsets land on the video's clock.

**Tell-tale signs of an uncorrected skew:**
- The per-chapter "GPS lock delay" (`gpsOffset`, before scaling) is large and suspiciously similar across all chapters — a real lock delay varies (cold vs warm starts), a clock skew is constant.
- After correcting, each `gpsStart` should fall within a few seconds of its chapter title (the first clip's time), reflecting a realistic warm-GPS lock.

**Measuring it:** pick a physical event visible in both — easiest is the car pulling out of a parked spot. Extract a frame to read the on-screen clock (`ffmpeg -ss <videoSeconds> -i NN.mp4 -frames:v 1 frame.png`; the encode crops the top, so the bottom timestamp survives), and find the GPS time where speed goes 0 → moving. `skew = gps_time − onscreen_time`.

**Fine-tuning:** the video is 3× sped, so **3 s of skew = 1 video-second** of marker shift. Less skew → the marker moves later (further behind the footage); more skew → earlier.
