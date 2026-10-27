# gst-playground — filmstrips with GStreamer

Builds filmstrip (sprite) sheets from a video with GStreamer and Go, as a
replacement for the production ffmpeg command. One decode writes three sheet
sizes, each a 10×10 grid:

| Profile | Folder | Frame size | File |
|---|---|---|---|
| large | `large/` | 427×240 | `<id>_l01.jpg`, `<id>_l02.jpg`, … |
| medium | `medium/` | 249×140 | `<id>_m01.jpg`, … |
| thumbnail | `thumbnail/` | 160×90 | `<id>_t01.jpg`, … |

`<id>` is the input file name without its extension. How many frames are
sampled comes from `calculateFPS(duration)`, the same rule the Go service uses:
about 10 frames for clips up to 10 s, rising to a maximum of 500 for long videos.

`ff-filmstrip.sh` runs the production ffmpeg command on the same input, for
comparison.

## Requirements

- Go 1.27+
- GStreamer 1.28+ with the base, good and bad plugins (`brew install gstreamer` on macOS)
- ffmpeg and ffprobe (only for `ff-filmstrip.sh`)

## Usage

```bash
go run . -input video.mp4 -out gst-filmstrip
```

Or build once and run the binary:

```bash
go build -o gst-playground .
./gst-playground -input video.mp4
```

Each run prints the duration, the sampling rate, how each sample was obtained,
and how long it took:

```
duration: 653.293s  fps: 0.1320700858  (one frame every 7.57s, ~86 frames)
mode:     keyframe  (samples: keyframe=34 accurate=52 [drift=51 repeat=1 stale=0] exact=0)
sheets:   large=1 medium=1 thumbnail=1 in gst-filmstrip/
2026/10/20 12:55:57 filmstrips made in 1.592s
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `-input` | a local sample movie | Video file. Set this; the default path only exists on the author's machine. |
| `-out` | `gst-filmstrip` | Output folder. Sheets go into `large/`, `medium/` and `thumbnail/` inside it. Old sheets for the same `<id>` are removed first. |
| `-mode` | `auto` | How frames are extracted; see [Modes](#modes). |
| `-max-drift` | `2s` | Keyframe and auto modes: how far a keyframe may be from the sample time. It's capped at half the sample interval. |
| `-deinterlace` | off | Deinterlace every frame before sampling. Use it only for interlaced sources (1080i, 576i, 480i). |
| `-threads` | `2` | Threads for software decoders (`avdec_*`); `0` = automatic. It has no effect on hardware decoders such as VideoToolbox. |

### Environment variables

| Variable | Description |
|---|---|
| `GST_PLUGIN_FEATURE_RANK=vtdec_hw:0,vtdec:0` | Disables the macOS hardware decoder, forcing software decoding (useful for comparisons). |
| `GST_DEBUG=2` | Prints GStreamer warnings. |

## Modes

| Mode | What it does | Use when |
|---|---|---|
| `exact` | Decodes every frame and samples with `videorate`. Picks the same frames as ffmpeg's `fps` filter. | The output must match production ffmpeg. |
| `keyframe` | Seeks once per sample and uses the nearest keyframe if it is within `-max-drift`. Otherwise it does an accurate seek to the exact frame. | Speed matters more than exact frames. |
| `auto` | Uses keyframe mode, but switches to exact mode for clips under 60 s, and when more than 30% of the first 10 samples share their keyframe with the previous sample (sparse keyframes or long GOPs). | The default. |

Keyframe mode handles these edge cases:

- **Sparse keyframes or long GOPs:** if a keyframe repeats the previous sample's, that sample uses an accurate seek. In auto mode, too many repeats switch the rest of the run to exact mode, which is cheaper than repeated seeks.
- **Short clips:** auto mode always uses exact mode for these. Decoding them fully is cheap, and they may have only one keyframe.
- **Stale frames:** some hardware decoders (VideoToolbox) output frames decoded before a backward seek. A frame is accepted only if it is the keyframe the demuxer seeked to; otherwise that sample uses an accurate seek.

## ffmpeg comparison script

```bash
./ff-filmstrip.sh [input] [output-dir]      # output-dir defaults to ff-filmstrip
DEINTERLACE=yadif ./ff-filmstrip.sh video.mp4
```

| Argument / variable | Default | Description |
|---|---|---|
| `$1` input | the same local sample movie | Video file |
| `$2` output dir | `ff-filmstrip` | Same `large/`, `medium/`, `thumbnail/` layout as the Go app |
| `DEINTERLACE` | empty | ffmpeg filter added before `fps`, e.g. `yadif` |

The script runs the production command without `-progress unix://` and the
HTTP/proxy input flags. `<fps>` comes from the same `calculateFPS` rule, and
`-t` is set to the full duration.

## How the GStreamer version maps to the ffmpeg command

| ffmpeg | GStreamer app |
|---|---|
| `-an` | `decodebin` stops before decoding audio |
| `-threads 2` | `max-threads` on software decoders |
| `[deinterlace,]` | `-deinterlace` → `deinterlace mode=interlaced` |
| `fps=<fps>` | `videorate`, starting where ffmpeg takes its first frame, so both pick the same frames |
| `scale=427:240:flags=bicubic` | `videoscale method=catrom add-borders=false` (stretches like ffmpeg) |
| `split=3`, `scale=249:140`, `scale=160:90` | `tee` with three branches; medium and thumbnail are scaled from the large frame |
| `tile=10x10` | Go tiles frames into 10×10 sheets; a short last page keeps the full grid with black empty cells |
| `-q:v 3` / `-q:v 4` | JPEG quality 80 / 77, chosen to match ffmpeg's file sizes |
| `out_range=full`, `yuvj420p` | Nothing needed: JPEG is full-range |

## Benchmarks

Apple M4, macOS, GStreamer 1.28.8, ffmpeg 9.0.2, Go 1.27.1. Times are wall
clock for the whole run, including probing the duration.

**Feature film:** 1920×800 H.264 in Matroska, 1 h 36 min, 23.976 fps, one
keyframe every 6.0 s on average. 500 samples, 5 sheets per profile.

| Tool | Time | Speed-up vs ffmpeg |
|---|---|---|
| ffmpeg (production command, software decode) | 196.2 s | 1× |
| GStreamer `-mode exact` (hardware decode) | 62.0 s | 3.2× |
| GStreamer `-mode auto` (default, 2 s drift) | 23.2 s | 8.5× |
| GStreamer `-max-drift 6s` | 9.1 s | 21.6× |

**Short clip (`test-src/test.mp4`):** 640×360 H.264, 10 min 53 s, 29.97 fps,
one keyframe every 5.2 s. 86 samples, 1 sheet per profile.

| Tool | Time | With deinterlacing |
|---|---|---|
| ffmpeg | 2.0–2.2 s | 17.0 s (`yadif`) |
| GStreamer `-mode exact` | 2.2–2.5 s | 2.7 s |
| GStreamer `-mode auto` | 1.6 s | 1.8 s |

**Synthetic edge cases:** 5 min clips at 25 fps, 73 samples; 8 s clip, 10 samples.

| Clip | `exact` | `keyframe` | `auto` (chooses) |
|---|---|---|---|
| keyframe every 2 s | 1.2 s | 0.56 s | 0.53 s (keyframe) |
| keyframe every 30 s | 1.1 s | 3.6 s | 1.9 s (switches to exact) |
| one keyframe in 5 min | 1.1 s | 28.8 s | 2.6 s (switches to exact) |
| 8 s clip | 0.55 s | 0.32 s | 0.24 s (exact) |

### Why GStreamer is faster

- **Hardware decoding:** `decodebin` picks VideoToolbox on macOS, while the
  ffmpeg command decodes in software on 2 threads. This alone explains the 3×
  gap in exact mode.
- **Fewer decoded frames:** keyframe mode decodes about one frame per sample
  instead of every frame. For the film that means about 500–1,000 frames
  instead of 139,397.
- **Cheap deinterlacing:** `deinterlace` costs far less than `yadif`, and
  both deinterlace every frame.

### Output comparison

- **Exact mode vs ffmpeg:** the same frames in the same cells (86 of 86 on
  `test.mp4`, checked against a clip that shows its timestamp). PSNR between
  the sheets is 36–40 dB, meaning visually identical; the remaining
  differences come from the scaling filter (catrom vs bicubic) and the JPEG
  encoder. File sizes are within 8%.
- **Keyframe and auto modes:** each cell is within `-max-drift` of the frame
  ffmpeg would pick.

## Project layout

| File | Purpose |
|---|---|
| `main.go` | Pipeline, extraction modes, sheet writing, flags |
| `macos_darwin.go` | Runs the code inside `gst_macos_main` on macOS |
| `macos_other.go` | No-op `runMain` for other platforms |
| `ff-filmstrip.sh` | ffmpeg comparison script |
| `test-src/` | Sample clips (not committed; add your own) |
