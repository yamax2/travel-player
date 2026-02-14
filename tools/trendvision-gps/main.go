package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func main() {
	speed := flag.Float64("speed", 1, "speed factor (e.g. 3 for 3x timelapse)")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "usage: trendvision-gps [-speed N] <file.mp4>\n")
		os.Exit(1)
	}
	file := flag.Arg(0)

	// Get PTS timestamps for each subtitle packet
	ptsOut, err := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "s:0",
		"-show_entries", "packet=pts_time",
		"-of", "csv=p=0",
		file,
	).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ffprobe failed: %v\n", err)
		os.Exit(1)
	}

	var pts []float64
	for _, line := range strings.Split(strings.TrimSpace(string(ptsOut)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		v, err := strconv.ParseFloat(line, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad PTS value %q: %v\n", line, err)
			os.Exit(1)
		}
		pts = append(pts, v)
	}

	if len(pts) == 0 {
		fmt.Println("0")
		return
	}

	// Dump raw subtitle stream as binary
	rawOut, err := exec.Command("ffmpeg",
		"-v", "error",
		"-i", file,
		"-map", "0:s:0",
		"-c", "copy",
		"-f", "rawvideo",
		"-",
	).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ffmpeg failed: %v\n", err)
		os.Exit(1)
	}

	// Parse mov_text packets: 2-byte big-endian length prefix + payload
	re := regexp.MustCompile(`\$G[NP]RMC,([^;*]+)`)
	buf := bytes.NewReader(rawOut)
	packetIdx := 0

	type trackpoint struct {
		lat, lon float64
		speed    float64
		course   float64
		date     string // YYYY-MM-DD
		sec      int    // GPX second = floor(PTS / speed_factor)
	}

	var points []trackpoint
	firstFixPTS := -1.0
	lastNMEATime := ""
	lastKey := ""

	// Derive GPX filename from MP4 path
	ext := filepath.Ext(file)
	gpxPath := file[:len(file)-len(ext)] + ".gpx"
	baseName := filepath.Base(file[:len(file)-len(ext)])

	for buf.Len() >= 2 {
		var length uint16
		if err := binary.Read(buf, binary.BigEndian, &length); err != nil {
			break
		}

		if int(length) > buf.Len() {
			break
		}

		payload := make([]byte, length)
		if _, err := buf.Read(payload); err != nil {
			break
		}

		m := re.FindSubmatch(payload)
		if m != nil {
			fields := strings.Split(string(m[1]), ",")
			// fields: 0=time, 1=status, 2=lat, 3=N/S, 4=lon, 5=E/W, 6=knots, 7=course, 8=date, ...
			if len(fields) >= 9 && fields[1] == "A" {
				if packetIdx >= len(pts) {
					packetIdx++
					continue
				}

				// Dedup by NMEA UTC time (integer seconds HHMMSS)
				nmeaTime := fields[0]
				if dotIdx := strings.Index(nmeaTime, "."); dotIdx >= 0 {
					nmeaTime = nmeaTime[:dotIdx]
				}
				if nmeaTime == lastNMEATime {
					packetIdx++
					continue
				}
				lastNMEATime = nmeaTime

				ptsVal := pts[packetIdx]
				if firstFixPTS < 0 {
					firstFixPTS = ptsVal
				}

				sec := int(math.Floor(ptsVal / *speed))

				lat := parseCoord(fields[2], fields[3])
				lon := parseCoord(fields[4], fields[5])
				knots, _ := strconv.ParseFloat(fields[6], 64)
				spd := knots * 1.852
				crs, _ := strconv.ParseFloat(fields[7], 64)
				date := parseDate(fields[8])

				// Skip if formatted values identical to previous point
				key := fmt.Sprintf("%.7f,%.7f,%.1f,%.1f", lat, lon, spd, crs)
				if key == lastKey {
					packetIdx++
					continue
				}
				lastKey = key

				points = append(points, trackpoint{
					lat:    lat,
					lon:    lon,
					speed:  spd,
					course: crs,
					date:   date,
					sec:    sec,
				})
			}
		}

		packetIdx++
	}

	// Write GPX file
	var gpx strings.Builder
	gpx.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	gpx.WriteString(`<gpx version="1.1" xmlns="http://www.topografix.com/GPX/1/1">` + "\n")
	gpx.WriteString("  <trk>\n")
	gpx.WriteString(fmt.Sprintf("    <name>%s</name>\n", baseName))
	gpx.WriteString("    <trkseg>\n")

	for _, p := range points {
		h := p.sec / 3600
		m := (p.sec % 3600) / 60
		s := p.sec % 60
		timeStr := fmt.Sprintf("%sT%02d:%02d:%02dZ", p.date, h, m, s)

		gpx.WriteString(fmt.Sprintf(
			"      <trkpt lat=\"%.7f\" lon=\"%.7f\"><time>%s</time><speed>%.1f</speed><course>%.1f</course></trkpt>\n",
			p.lat, p.lon, timeStr, p.speed, p.course,
		))
	}

	gpx.WriteString("    </trkseg>\n")
	gpx.WriteString("  </trk>\n")
	gpx.WriteString("</gpx>\n")

	if err := os.WriteFile(gpxPath, []byte(gpx.String()), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write GPX: %v\n", err)
		os.Exit(1)
	}

	// Print gpsOffset
	if firstFixPTS >= 0 {
		fmt.Printf("%.1f\n", firstFixPTS / *speed)
	} else {
		fmt.Println("0")
	}
}

// parseCoord converts NMEA DDDMM.MMMM,H to decimal degrees.
func parseCoord(raw, hemisphere string) float64 {
	if raw == "" {
		return 0
	}
	// Find the decimal point to split degrees and minutes
	dotIdx := strings.Index(raw, ".")
	if dotIdx < 2 {
		return 0
	}
	degStr := raw[:dotIdx-2]
	minStr := raw[dotIdx-2:]
	deg, _ := strconv.ParseFloat(degStr, 64)
	min, _ := strconv.ParseFloat(minStr, 64)
	result := deg + min/60.0
	if hemisphere == "S" || hemisphere == "W" {
		result = -result
	}
	return result
}

// parseDate converts NMEA DDMMYY to YYYY-MM-DD.
func parseDate(raw string) string {
	if len(raw) < 6 {
		return "1970-01-01"
	}
	dd := raw[0:2]
	mm := raw[2:4]
	yy, _ := strconv.Atoi(raw[4:6])
	var year int
	if yy < 80 {
		year = 2000 + yy
	} else {
		year = 1900 + yy
	}
	return fmt.Sprintf("%d-%s-%s", year, mm, dd)
}
