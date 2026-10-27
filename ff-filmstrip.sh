#!/usr/bin/env bash
# Builds filmstrip sheets with the production ffmpeg command, so the result and
# timing can be compared with the GStreamer version in main.go. One pass writes
# three 10x10 sheet sizes, each in its own folder under the output dir:
#   large/<id>_lNN.jpg      427x240 frames
#   medium/<id>_mNN.jpg     249x140 frames
#   thumbnail/<id>_tNN.jpg  160x90 frames
# Usage:
#   ./ff-filmstrip.sh [input] [output-dir]
# Set DEINTERLACE=yadif for interlaced sources.
set -euo pipefail

INPUT="${1:-/Users/paullaidlaw/Workbench/Sample-Media/movie/The.Incomer.2026.1080p.ZF.webm}"
OUT_DIR="${2:-ff-filmstrip}"
DEINTERLACE="${DEINTERLACE:-}"

for cmd in ffmpeg ffprobe perl; do
	command -v "$cmd" >/dev/null || { echo "error: $cmd not found" >&2; exit 1; }
done
[[ -f "$INPUT" ]] || { echo "error: no such file: $INPUT" >&2; exit 1; }

# Wall-clock time in seconds with milliseconds (macOS date has no %N).
now() { perl -MTime::HiRes=time -e 'printf "%.3f", time'; }

ID=$(basename "${INPUT%.*}")

start=$(now)

duration=$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$INPUT")

# Same rule as calculateFPS in the Go service: fps is the output sampling rate.
fps=$(awk -v d="$duration" 'BEGIN {
	if (d <= 0) { exit 1 }
	fps = 0.1
	if      (d <= 10)   fps = 10 / d
	else if (d <= 100)  fps = (d / 2 + 15) / d
	else if (d <= 1000) fps = ((d - 100) / 26 + 65) / d
	else if (d >= 5000) fps = 500 / d
	printf "%.10f", fps
}') || { echo "error: invalid duration '$duration'" >&2; exit 1; }

LARGE="$OUT_DIR/large"
MEDIUM="$OUT_DIR/medium"
THUMBNAIL="$OUT_DIR/thumbnail"
mkdir -p "$LARGE" "$MEDIUM" "$THUMBNAIL"
# Remove sheets from a previous run so a shorter video doesn't leave stale ones.
rm -f "$LARGE/${ID}"_l[0-9][0-9].jpg "$MEDIUM/${ID}"_m[0-9][0-9].jpg "$THUMBNAIL/${ID}"_t[0-9][0-9].jpg

ffmpeg -hide_banner -loglevel error -nostdin \
	-threads 2 -filter_threads 1 -filter_complex_threads 1 \
	-y -an \
	-t "$duration" \
	-i "$INPUT" \
	-filter_complex "${DEINTERLACE:+$DEINTERLACE,}fps=${fps},scale=427:240:flags=bicubic:out_range=full,format=yuvj420p,split=3[s0][s1][s2];
		[s0]tile=10x10[tile_l];
		[s1]scale=249:140:flags=bicubic,tile=10x10[tile_m];
		[s2]scale=160:90:flags=bicubic,tile=10x10[tile_t]" \
	-map "[tile_l]" -q:v 3 "$LARGE/${ID}_l%02d.jpg" \
	-map "[tile_m]" -q:v 3 "$MEDIUM/${ID}_m%02d.jpg" \
	-map "[tile_t]" -q:v 4 "$THUMBNAIL/${ID}_t%02d.jpg"

end=$(now)

count() { find "$1" -name "${ID}_$2[0-9][0-9].jpg" | wc -l | tr -d ' '; }
echo "input:    $INPUT"
printf 'duration: %.3fs  fps: %s  (one frame every %.2fs, ~%.0f frames)\n' \
	"$duration" "$fps" "$(awk -v f="$fps" 'BEGIN{print 1/f}')" "$(awk -v d="$duration" -v f="$fps" 'BEGIN{print d*f}')"
echo "sheets:   large=$(count "$LARGE" l) medium=$(count "$MEDIUM" m) thumbnail=$(count "$THUMBNAIL" t) in $OUT_DIR/"
printf 'filmstrips made in %.3fs\n' "$(awk -v s="$start" -v e="$end" 'BEGIN{print e-s}')"
