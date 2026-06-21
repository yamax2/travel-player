package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type trackpoint struct {
	epoch   int64
	lat     float64
	lon     float64
	heading float64
	speed   float64
}

type video struct {
	filename string
	seq      int
	startUTC time.Time
	duration float64 // seconds
}

type chapter struct {
	Title     string  `json:"title"`
	Start     float64 `json:"start"`
	End       float64 `json:"end"`
	GPSOffset float64 `json:"gpsOffset,omitempty"`
	GPSStart  string  `json:"gpsStart,omitempty"` // absolute UTC time of the chapter's first GPS fix
	hasGPS    bool
}

type tripJSON struct {
	Title    string    `json:"title"`
	Video    string    `json:"video"`
	Speed    int       `json:"speed"`
	TZ       string    `json:"tz"`
	GPX      string    `json:"gpx"`
	Chapters []chapter `json:"chapters"`
}

func main() {
	tzFlag := flag.String("tz", "+05:00", "local timezone offset (e.g. +05:00)")
	inputFlag := flag.String("input", "/Volumes/NO NAME", "path to dashcam SD card")
	outputFlag := flag.String("output", ".", "directory for output files")
	skewFlag := flag.Int("skew", 0, "seconds the GPS clock runs AHEAD of the dashcam video clock (RTC). "+
		"70mai units often log GPS ~2 min ahead of the burned-in/file timestamps; subtract it so the "+
		"track lines up with the footage. Measure by comparing a physical event (e.g. the car pulling out) "+
		"in the video's on-screen clock vs the GPS speed going 0->moving.")
	flag.Parse()

	// Parse timezone offset
	tzOffset, err := parseTZOffset(*tzFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad -tz value %q: %v\n", *tzFlag, err)
		os.Exit(1)
	}

	// 1. Parse GPS data
	gpsPoints := parseGPSFiles(*inputFlag)
	// Correct GPS-vs-RTC clock skew: shift GPS timestamps back onto the video's
	// (RTC) clock so points line up with the footage and the per-chapter offsets
	// reflect the real GPS lock delay rather than the constant skew.
	if *skewFlag != 0 {
		for i := range gpsPoints {
			gpsPoints[i].epoch -= int64(*skewFlag)
		}
		fmt.Fprintf(os.Stderr, "Applied GPS clock skew: -%ds\n", *skewFlag)
	}
	sort.Slice(gpsPoints, func(i, j int) bool { return gpsPoints[i].epoch < gpsPoints[j].epoch })
	fmt.Fprintf(os.Stderr, "Parsed %d GPS points\n", len(gpsPoints))

	if len(gpsPoints) > 0 {
		t0 := time.Unix(gpsPoints[0].epoch, 0).UTC()
		t1 := time.Unix(gpsPoints[len(gpsPoints)-1].epoch, 0).UTC()
		fmt.Fprintf(os.Stderr, "GPS range: %s .. %s\n", t0.Format(time.RFC3339), t1.Format(time.RFC3339))
	}

	// 2. Parse video files
	videos := parseVideos(*inputFlag, tzOffset)
	sort.Slice(videos, func(i, j int) bool { return videos[i].seq < videos[j].seq })
	fmt.Fprintf(os.Stderr, "Found %d videos\n", len(videos))

	if len(videos) == 0 {
		fmt.Fprintf(os.Stderr, "No videos found in %s/Normal/Front/\n", *inputFlag)
		os.Exit(1)
	}

	// 3. Group into trips (10 min gap threshold)
	trips := groupTrips(videos)
	fmt.Fprintf(os.Stderr, "Detected %d trips\n", len(trips))

	// 4. Process — one video, one GPX, trips become chapters
	os.MkdirAll(*outputFlag, 0755)
	outputName := filepath.Base(*outputFlag) // e.g. "03"

	var chapters []chapter
	var allPoints []trackpoint
	cumStart := 0.0

	for tripIdx, trip := range trips {
		tripNum := tripIdx + 1
		fmt.Fprintf(os.Stderr, "\n--- Trip %d (chapter): %d videos ---\n", tripNum, len(trip))

		// Determine chapter title from first video with a plausible date
		chapterTitle := fmt.Sprintf("Trip %d", tripNum)
		for _, v := range trip {
			if hasPlausibleDate(v, gpsPoints) {
				chapterTitle = v.startUTC.Format("15:04")
				break
			}
		}

		// Sum durations for this trip, collect GPS points
		tripStart := cumStart
		var tripHasGPS bool
		var firstGPSOffset float64
		var firstGPSEpoch int64
		firstGPSOffsetSet := false

		for _, v := range trip {
			startEpoch := v.startUTC.Unix()
			endEpoch := startEpoch + int64(math.Ceil(v.duration))
			chapterPoints := findPointsInRange(gpsPoints, startEpoch, endEpoch)

			if len(chapterPoints) > 0 {
				if !firstGPSOffsetSet {
					// gpsOffset for the chapter = time (in VIDEO seconds) from the chapter's
					// video start to its first GPS fix. cumStart-tripStart is already video
					// seconds; the in-clip lock delay is real seconds, so scale it by 1/3.
					firstGPSOffset = cumStart - tripStart + float64(chapterPoints[0].epoch-startEpoch)/3.0
					firstGPSEpoch = chapterPoints[0].epoch
					firstGPSOffsetSet = true
				}
				tripHasGPS = true
				allPoints = append(allPoints, chapterPoints...)
				fmt.Fprintf(os.Stderr, "  %s: %d GPS points\n", v.filename, len(chapterPoints))
			} else {
				fmt.Fprintf(os.Stderr, "  %s: no GPS\n", v.filename)
			}

			cumStart += v.duration / 3.0
		}

		ch := chapter{
			Title: chapterTitle,
			Start: round1(tripStart),
			End:   round1(cumStart),
		}
		if tripHasGPS {
			ch.GPSOffset = round1(firstGPSOffset)
			ch.GPSStart = time.Unix(firstGPSEpoch, 0).UTC().Format("2006-01-02T15:04:05Z")
			ch.hasGPS = true
		}
		chapters = append(chapters, ch)
	}

	// Write single GPX
	gpxName := outputName + ".gpx"
	if len(allPoints) > 0 {
		filled := fillGaps(allPoints)
		writeGPX(filepath.Join(*outputFlag, gpxName), outputName, filled)
		fmt.Fprintf(os.Stderr, "\nWrote %s (%d trackpoints)\n", gpxName, len(filled))
	}

	// Write single ffmpeg script
	scriptName := outputName + ".sh"
	writeFFmpegScript(filepath.Join(*outputFlag, scriptName), videos, *inputFlag, outputName)
	fmt.Fprintf(os.Stderr, "Wrote %s\n", scriptName)

	// Write index.json
	firstDate := ""
	for _, v := range videos {
		if hasPlausibleDate(v, gpsPoints) {
			firstDate = v.startUTC.Format("2006-01-02")
			break
		}
	}

	idx := tripJSON{
		Title:    firstDate,
		Video:    outputName + ".mp4",
		Speed:    3,
		TZ:       *tzFlag,
		GPX:      gpxName,
		Chapters: chapters,
	}
	writeJSON(filepath.Join(*outputFlag, "index.json"), idx)
	fmt.Fprintf(os.Stderr, "Wrote index.json\n")
}

func parseTZOffset(tz string) (time.Duration, error) {
	// Parse "+05:00" or "-03:30" etc.
	if len(tz) < 5 {
		return 0, fmt.Errorf("too short")
	}
	sign := 1
	if tz[0] == '-' {
		sign = -1
	} else if tz[0] != '+' {
		return 0, fmt.Errorf("must start with + or -")
	}
	parts := strings.Split(tz[1:], ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM format")
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}
	return time.Duration(sign) * (time.Duration(h)*time.Hour + time.Duration(m)*time.Minute), nil
}

func parseGPSFiles(inputDir string) []trackpoint {
	pattern := filepath.Join(inputDir, "GPSData*.txt")
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No GPS files matching %s\n", pattern)
		return nil
	}

	var points []trackpoint
	for _, path := range matches {
		points = append(points, parseGPSFile(path)...)
	}
	return points
}

func parseGPSFile(path string) []trackpoint {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open %s: %v\n", path, err)
		return nil
	}
	defer f.Close()

	var points []trackpoint
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "$") {
			continue
		}

		fields := strings.Split(line, ",")
		if len(fields) < 6 {
			continue
		}

		// Skip void status
		if fields[1] != "A" {
			continue
		}

		rawTS, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// Dashcam stores CST-biased epoch; add 8h to get UTC
		utcEpoch := rawTS + 8*3600

		lat, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			continue
		}
		lon, err := strconv.ParseFloat(fields[3], 64)
		if err != nil {
			continue
		}

		// Skip zero coordinates
		if lat == 0 && lon == 0 {
			continue
		}

		heading, _ := strconv.ParseFloat(fields[4], 64)
		heading /= 100.0 // centidegrees to degrees

		speedRaw, _ := strconv.ParseFloat(fields[5], 64)
		speed := speedRaw * 0.036 // cm/s to km/h

		points = append(points, trackpoint{
			epoch:   utcEpoch,
			lat:     lat,
			lon:     lon,
			heading: heading,
			speed:   speed,
		})
	}
	return points
}

var videoRe = regexp.MustCompile(`NO(\d{4})(\d{2})(\d{2})-(\d{2})(\d{2})(\d{2})-(\d{6})F\.MP4`)

func parseVideos(inputDir string, tzOffset time.Duration) []video {
	pattern := filepath.Join(inputDir, "Normal", "Front", "*.MP4")
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "No videos matching %s\n", pattern)
		return nil
	}

	var videos []video
	for _, path := range matches {
		name := filepath.Base(path)
		// Skip macOS resource fork files
		if strings.HasPrefix(name, "._") {
			continue
		}
		m := videoRe.FindStringSubmatch(name)
		if m == nil {
			fmt.Fprintf(os.Stderr, "Skipping %s: doesn't match pattern\n", name)
			continue
		}

		year, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		hour, _ := strconv.Atoi(m[4])
		min, _ := strconv.Atoi(m[5])
		sec, _ := strconv.Atoi(m[6])
		seqNum, _ := strconv.Atoi(m[7])

		localTime := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.UTC)
		utcTime := localTime.Add(-tzOffset)

		dur := getVideoDuration(path)

		videos = append(videos, video{
			filename: name,
			seq:      seqNum,
			startUTC: utcTime,
			duration: dur,
		})
	}
	return videos
}

func getVideoDuration(path string) float64 {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "csv=p=0",
		path,
	).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ffprobe failed for %s: %v\n", path, err)
		return 180.0 // fallback 3 minutes
	}
	s := strings.TrimSpace(string(out))
	d, err := strconv.ParseFloat(s, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Bad duration %q for %s: %v\n", s, path, err)
		return 180.0
	}
	return d
}

func groupTrips(videos []video) [][]video {
	if len(videos) == 0 {
		return nil
	}

	const gapThreshold = 10 * 60     // 10 minutes — normal trip break
	const badTSThreshold = 24 * 3600 // 24 hours — implausible gap means bad timestamp (e.g. pre-GPS-lock default date)

	var trips [][]video
	current := []video{videos[0]}

	for i := 1; i < len(videos); i++ {
		prev := videos[i-1]
		prevEnd := prev.startUTC.Add(time.Duration(prev.duration * float64(time.Second)))
		gap := videos[i].startUTC.Sub(prevEnd).Seconds()

		// Negative gap or gap > 24h means a bad timestamp — keep in same trip.
		// Normal gap >= 10 min starts a new trip.
		if gap >= gapThreshold && gap < badTSThreshold {
			trips = append(trips, current)
			current = nil
		}
		current = append(current, videos[i])
	}
	trips = append(trips, current)

	return trips
}

// hasPlausibleDate checks if a video's timestamp is within 24h of the GPS data range.
// Videos recorded before GPS lock may have a default date years off.
func hasPlausibleDate(v video, gpsPoints []trackpoint) bool {
	if len(gpsPoints) == 0 {
		return true // no GPS to compare against, assume ok
	}
	gpsStart := time.Unix(gpsPoints[0].epoch, 0).UTC()
	gpsEnd := time.Unix(gpsPoints[len(gpsPoints)-1].epoch, 0).UTC()
	margin := 24 * time.Hour
	return v.startUTC.After(gpsStart.Add(-margin)) && v.startUTC.Before(gpsEnd.Add(margin))
}

func findPointsInRange(points []trackpoint, startEpoch, endEpoch int64) []trackpoint {
	// Binary search for start
	lo := sort.Search(len(points), func(i int) bool { return points[i].epoch >= startEpoch })

	var result []trackpoint
	for i := lo; i < len(points); i++ {
		if points[i].epoch >= endEpoch {
			break
		}
		result = append(result, points[i])
	}
	return result
}

func fillGaps(points []trackpoint) []trackpoint {
	if len(points) == 0 {
		return nil
	}

	const maxGapFill = 5 // only fill gaps up to 5 seconds

	var filled []trackpoint
	for i, p := range points {
		if i > 0 {
			prev := points[i-1]
			gap := p.epoch - prev.epoch
			if gap > 1 && gap <= maxGapFill {
				for epoch := prev.epoch + 1; epoch < p.epoch; epoch++ {
					filled = append(filled, trackpoint{
						epoch:   epoch,
						lat:     prev.lat,
						lon:     prev.lon,
						heading: prev.heading,
						speed:   prev.speed,
					})
				}
			}
		}
		filled = append(filled, p)
	}
	return filled
}

func writeGPX(path, name string, points []trackpoint) {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<gpx version=\"1.1\" xmlns=\"http://www.topografix.com/GPX/1/1\">\n")
	b.WriteString("  <trk>\n")
	b.WriteString(fmt.Sprintf("    <name>%s</name>\n", name))
	b.WriteString("    <trkseg>\n")

	for _, p := range points {
		t := time.Unix(p.epoch, 0).UTC()
		b.WriteString(fmt.Sprintf(
			"      <trkpt lat=\"%.7f\" lon=\"%.7f\"><time>%s</time><speed>%.1f</speed><course>%.1f</course></trkpt>\n",
			p.lat, p.lon, t.Format("2006-01-02T15:04:05Z"), p.speed, p.heading,
		))
	}

	b.WriteString("    </trkseg>\n")
	b.WriteString("  </trk>\n")
	b.WriteString("</gpx>\n")

	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write %s: %v\n", path, err)
	}
}

func writeFFmpegScript(path string, videos []video, inputDir string, outputName string) {
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -e\n\n")
	b.WriteString(`ffmpeg -f concat -safe 0 -i <(cat <<'EOF'`)
	b.WriteString("\n")

	for _, v := range videos {
		srcPath := filepath.Join(inputDir, "Normal", "Front", v.filename)
		b.WriteString(fmt.Sprintf("file '%s'\n", srcPath))
	}

	b.WriteString("EOF\n")
	b.WriteString(fmt.Sprintf(`) -filter:v "setpts=PTS/3,crop=in_w:in_h-262:0:262" -c:v libsvtav1 -crf 34 -preset 4 -an "%s.mp4"`, outputName))
	b.WriteString("\n")

	if err := os.WriteFile(path, []byte(b.String()), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write %s: %v\n", path, err)
	}
}

func writeJSON(path string, data tripJSON) {
	// Use *float64 for gpsOffset so it's omitted when nil but present when 0
	type chapterOut struct {
		Title     string   `json:"title"`
		Start     float64  `json:"start"`
		End       float64  `json:"end"`
		GPSOffset *float64 `json:"gpsOffset,omitempty"`
		GPSStart  string   `json:"gpsStart,omitempty"`
	}

	type jsonOut struct {
		Title    string       `json:"title"`
		Video    string       `json:"video"`
		Speed    int          `json:"speed"`
		TZ       string       `json:"tz"`
		GPX      string       `json:"gpx"`
		Chapters []chapterOut `json:"chapters"`
	}

	out := jsonOut{
		Title: data.Title,
		Video: data.Video,
		Speed: data.Speed,
		TZ:    data.TZ,
		GPX:   data.GPX,
	}

	for _, ch := range data.Chapters {
		co := chapterOut{
			Title: ch.Title,
			Start: ch.Start,
			End:   ch.End,
		}
		if ch.hasGPS {
			offset := ch.GPSOffset
			co.GPSOffset = &offset
			co.GPSStart = ch.GPSStart
		}
		out.Chapters = append(out.Chapters, co)
	}

	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "JSON marshal error: %v\n", err)
		return
	}

	if err := os.WriteFile(path, append(raw, '\n'), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write %s: %v\n", path, err)
	}
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
