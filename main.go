package main

import (
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gst/go-gst/pkg/gst"
	"github.com/go-gst/go-gst/pkg/gstapp"
)

// profile is one sheet size, matching one output of the production ffmpeg
// command (tile=10x10, -q:v per output).
type profile struct {
	folder        string // subfolder of the output folder for this profile
	suffix        string // file name suffix: <id>_<suffix>NN.jpg
	width, height int    // size of one frame on the sheet
	quality       int    // JPEG quality, 1-100
}

// Frames for "l" are scaled from the source; "m" and "t" are scaled down from
// the "l" frames, as ffmpeg does after split=3. Qualities were picked to give
// about the same file sizes as ffmpeg's -q:v 3 (l, m) and -q:v 4 (t).
var profiles = []profile{
	{folder: "large", suffix: "l", width: 427, height: 240, quality: 80},
	{folder: "medium", suffix: "m", width: 249, height: 140, quality: 80},
	{folder: "thumbnail", suffix: "t", width: 160, height: 90, quality: 77},
}

const (
	sheetCols = 10
	sheetRows = 10
)

// calculateFPS is the Go service's sampling rule: it returns the output rate
// in frames per second of video, and the seconds between frames.
func calculateFPS(duration float64) (float64, float64) {
	fps := 0.1
	switch {
	case duration <= 10:
		fps = 10.0 / duration
	case duration <= 100:
		fps = (duration/2 + 15) / duration
	case duration <= 1000:
		fps = ((duration-100)/26 + 65) / duration
	case duration >= 5000:
		fps = 500 / duration
	}
	return fps, 1 / fps
}

func removeGlob(dir, pattern string) error {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return fmt.Errorf("glob %s: %w", pattern, err)
	}
	for _, f := range matches {
		if err := os.Remove(f); err != nil {
			return fmt.Errorf("remove %s: %w", f, err)
		}
	}
	return nil
}

// sheetWriter tiles raw RGBA frames into cols x rows pages and writes each page
// as a JPEG as soon as it is full. Like ffmpeg's tile filter, a short last page
// keeps the full grid size with the empty cells black. Pages are encoded in
// the background; encoders is shared between writers to cap the total.
type sheetWriter struct {
	prefix         string // path without the page number, e.g. out/test_l
	cols, rows     int
	frameW, frameH int
	quality        int
	sheet          *image.RGBA
	count          int // frames on the current page
	sheets         []string
	encoders       chan struct{}
	wg             sync.WaitGroup
	errMu          sync.Mutex
	err            error
}

func newSheetWriter(prefix string, p profile, encoders chan struct{}) *sheetWriter {
	return &sheetWriter{
		prefix:   prefix,
		cols:     sheetCols,
		rows:     sheetRows,
		frameW:   p.width,
		frameH:   p.height,
		quality:  p.quality,
		encoders: encoders,
	}
}

// add copies one frame of tightly packed RGBA pixels into the next free cell.
func (w *sheetWriter) add(pix []byte) error {
	rowBytes := w.frameW * 4
	if len(pix) < rowBytes*w.frameH {
		return fmt.Errorf("frame buffer too small: got %d bytes, want %d", len(pix), rowBytes*w.frameH)
	}

	if w.sheet == nil {
		w.sheet = image.NewRGBA(image.Rect(0, 0, w.cols*w.frameW, w.rows*w.frameH))
		draw.Draw(w.sheet, w.sheet.Bounds(), image.Black, image.Point{}, draw.Src)
	}

	x := (w.count % w.cols) * w.frameW
	y := (w.count / w.cols) * w.frameH
	for row := 0; row < w.frameH; row++ {
		dst := w.sheet.PixOffset(x, y+row)
		copy(w.sheet.Pix[dst:dst+rowBytes], pix[row*rowBytes:(row+1)*rowBytes])
	}

	w.count++
	if w.count == w.cols*w.rows {
		w.flush()
	}
	return w.firstErr()
}

// flush hands the current page to a background encoder.
func (w *sheetWriter) flush() {
	if w.count == 0 {
		return
	}

	img := w.sheet
	out := fmt.Sprintf("%s%02d.jpg", w.prefix, len(w.sheets)+1)
	w.sheets = append(w.sheets, out)
	w.sheet, w.count = nil, 0

	w.encoders <- struct{}{} // blocks while all encoders are busy
	w.wg.Add(1)
	go func() {
		defer func() {
			<-w.encoders
			w.wg.Done()
		}()
		if err := writeJPEG(out, img, w.quality); err != nil {
			w.setErr(err)
		}
	}()
}

// finish writes the last partial page and waits for all encoders.
func (w *sheetWriter) finish() ([]string, error) {
	w.flush()
	w.wg.Wait()
	if err := w.firstErr(); err != nil {
		return nil, err
	}
	return w.sheets, nil
}

func (w *sheetWriter) setErr(err error) {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	if w.err == nil {
		w.err = err
	}
}

func (w *sheetWriter) firstErr() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.err
}

// probeDuration returns the length of the video in seconds (like ffprobe).
// It only parses the container, without decoding.
func probeDuration(input string) (float64, error) {
	elem, err := gst.ParseLaunch("filesrc name=src ! parsebin ! fakesink")
	if err != nil {
		return 0, fmt.Errorf("build probe pipeline: %w", err)
	}
	pipeline := elem.(gst.Pipeline)
	pipeline.GetByName("src").SetObjectProperty("location", input)
	defer pipeline.SetState(gst.StateNull)

	pipeline.SetState(gst.StatePaused)
	if _, _, ret := pipeline.GetState(gst.ClockTime(30 * time.Second)); ret == gst.StateChangeFailure {
		if msg := pipeline.GetBus().PopFiltered(gst.MessageError); msg != nil {
			debug, gstErr := msg.ParseError()
			return 0, fmt.Errorf("probe %s: %w (%s)", input, gstErr, debug)
		}
		return 0, fmt.Errorf("probe %s: failed to open", input)
	}

	d, ok := pipeline.QueryDuration(gst.FormatTime)
	if !ok || d <= 0 {
		return 0, fmt.Errorf("probe %s: unknown duration", input)
	}
	return float64(d) / float64(time.Second), nil
}

// Extraction modes.
const (
	// modeExact decodes every frame and samples with videorate. It picks the
	// same frames as ffmpeg's fps filter.
	modeExact = "exact"
	// modeKeyframe seeks to the nearest keyframe for each sample, so only about
	// one frame per sample is decoded. It falls back to an accurate seek when
	// the keyframe is too far from the sample time or repeats the previous one.
	modeKeyframe = "keyframe"
	// modeAuto uses keyframe mode, but switches to exact mode for short clips
	// and when keyframes are spaced about a sample interval or more apart
	// (sparse keyframes, long GOPs). Each accurate seek then decodes about as
	// many frames as decoding straight through, so seeking stops paying off.
	modeAuto = "auto"
)

const (
	// Clips shorter than this are always decoded in exact mode: it's cheap, and
	// they may have only one or two keyframes.
	autoMinDuration = 60 * time.Second
	// Auto mode judges keyframe spacing after this many samples...
	autoMinSamples = 10
	// ...and switches to exact mode when more than this share of samples had
	// the same nearest keyframe as the previous sample. Fallbacks for drift
	// alone don't count: their accurate seek decodes less than one GOP, which
	// is still cheaper than decoding the whole sample interval.
	autoMaxRepeats = 0.3
	// Some decoders (e.g. vtdec) let frames decoded before a flushing seek out
	// after it. Those stale frames come from just after the previous sample, so
	// a keyframe is only used when it is this far past the previous sample and
	// the frame returned really is that keyframe.
	staleMargin = 500 * time.Millisecond
)

type extractOptions struct {
	fps         float64       // output frames per second of video
	duration    time.Duration // length of the video
	mode        string        // modeAuto, modeExact or modeKeyframe
	maxDrift    time.Duration // how far a keyframe may be from the sample time
	deinterlace bool
	threads     int
}

// extractStats describes how the frames were obtained.
type extractStats struct {
	mode      string // mode that produced the frames, e.g. "keyframe+exact"
	keyframes int    // samples taken from the nearest keyframe
	drifted   int    // accurate seeks because the keyframe was too far away
	repeated  int    // accurate seeks because the keyframe was not after the previous sample
	stale     int    // accurate seeks because the decoder returned a stale frame
	exact     int    // samples taken in exact mode
}

// sampleTime returns where sample k is taken: for output slot k, ffmpeg's fps
// filter takes the last frame before (k+0.5)/fps. Aiming half a source frame
// earlier makes the nearest frame (videorate) or the frame covering the time
// (accurate seek) that same frame. srcFrame is the source frame duration, or 0
// if unknown.
func sampleTime(k int, fps float64, srcFrame time.Duration) time.Duration {
	t := time.Duration((float64(k) + 0.5) / fps * float64(time.Second))
	t -= srcFrame / 2
	return max(t, 0)
}

// frameDuration returns the frame duration from caps, or 0 if unknown.
func frameDuration(caps *gst.Caps) time.Duration {
	if caps == nil || caps.GetSize() == 0 {
		return 0
	}
	num, den, ok := caps.GetStructure(0).GetFraction("framerate")
	if !ok || num <= 0 || den <= 0 {
		return 0
	}
	return time.Duration(float64(den) / float64(num) * float64(time.Second))
}

// buildPipeline builds the decode pipeline that mirrors:
//
//	ffmpeg -threads N -an -i input -filter_complex
//	  "[deinterlace,]fps=F,scale=427:240:flags=bicubic,split=3 ... tile=10x10"
//
// It returns one appsink per profile. With fps 0 there is no videorate and
// every decoded frame reaches the sinks (used by keyframe mode, which seeks).
func buildPipeline(input string, o extractOptions, fps float64) (gst.Pipeline, []gstapp.AppSink, error) {
	l := profiles[0]

	// add-borders=false stretches to the exact size like ffmpeg's scale, and
	// catrom is the closest videoscale method to ffmpeg's bicubic.
	const scale = "videoscale method=catrom add-borders=false"
	desc := "filesrc name=src ! decodebin name=dec ! "
	if o.deinterlace {
		// Like yadif's default, deinterlace every frame, before fps sampling.
		desc += "deinterlace mode=interlaced ! "
	}
	queue := "queue"
	if fps > 0 {
		// videorate drops frames, or duplicates them when fps is above the
		// source rate (short clips), like ffmpeg's fps filter.
		num, den := gst.UtilDoubleToFraction(fps)
		desc += fmt.Sprintf("videorate name=rate ! video/x-raw,framerate=%d/%d ! ", num, den)
	} else {
		// After each seek only one frame is wanted. Small queues stop the
		// decoder from running ahead and decoding frames that are then flushed.
		queue = "queue max-size-buffers=1"
	}
	desc += fmt.Sprintf("videoconvert ! %s ! video/x-raw,format=RGBA,width=%d,height=%d ! tee name=split ",
		scale, l.width, l.height)
	for i, p := range profiles {
		desc += "split. ! " + queue + " ! "
		if i > 0 {
			desc += fmt.Sprintf("%s ! video/x-raw,width=%d,height=%d ! ", scale, p.width, p.height)
		}
		desc += fmt.Sprintf("appsink name=sink_%s sync=false ", p.suffix)
	}

	elem, err := gst.ParseLaunch(desc)
	if err != nil {
		return nil, nil, fmt.Errorf("build pipeline (is a plugin missing?): %w", err)
	}
	pipeline := elem.(gst.Pipeline)

	pipeline.GetByName("src").SetObjectProperty("location", input)

	// -an: stop decodebin at encoded audio, so audio is never decoded. The
	// encoded audio pad is left unlinked and ignored.
	pipeline.GetByName("dec").Connect("autoplug-continue", func(_ gst.Element, _ gst.Pad, caps *gst.Caps) bool {
		return caps.GetSize() == 0 || !strings.HasPrefix(caps.GetStructure(0).GetName(), "audio/")
	})

	// -threads: limit software decoders. Hardware decoders (e.g. vtdec on
	// macOS) have no thread setting.
	pipeline.ConnectDeepElementAdded(func(_ gst.Bin, _ gst.Bin, e gst.Element) {
		if f := e.GetFactory(); f != nil && strings.HasPrefix(f.GetName(), "avdec_") {
			e.SetObjectProperty("max-threads", int32(o.threads))
		}
	})

	sinks := make([]gstapp.AppSink, len(profiles))
	for i, p := range profiles {
		sinks[i] = pipeline.GetByName("sink_" + p.suffix).(gstapp.AppSink)
		sinks[i].SetCaps(gst.CapsFromString(fmt.Sprintf(
			"video/x-raw,format=RGBA,width=%d,height=%d", p.width, p.height)))
	}
	return pipeline, sinks, nil
}

// busError returns the error message posted on the bus, if any.
func busError(pipeline gst.Pipeline) error {
	if msg := pipeline.GetBus().PopFiltered(gst.MessageError); msg != nil {
		debug, gstErr := msg.ParseError()
		return fmt.Errorf("GStreamer: %w (%s)", gstErr, debug)
	}
	return nil
}

// pause sets the pipeline to PAUSED and waits until it has prerolled. The
// element that failed (e.g. filesrc on a missing file) posts the reason on the bus.
func pause(pipeline gst.Pipeline) error {
	pipeline.SetState(gst.StatePaused)
	if _, _, ret := pipeline.GetState(gst.ClockTimeNone); ret == gst.StateChangeFailure {
		if err := busError(pipeline); err != nil {
			return err
		}
		return fmt.Errorf("failed to set pipeline to paused")
	}
	return nil
}

// addSample copies the frame in sample to w.
func addSample(w *sheetWriter, sample *gst.Sample) error {
	info, ok := sample.GetBuffer().Map(gst.MapRead)
	if !ok {
		return fmt.Errorf("map frame buffer")
	}
	defer info.Unmap()
	return w.add(info.Data())
}

// gstExtractFrames samples the video and passes the frames to one sheetWriter
// per profile, using the extraction mode in o.
func gstExtractFrames(input string, writers []*sheetWriter, o extractOptions) (extractStats, error) {
	if o.mode == modeExact || (o.mode == modeAuto && o.duration < autoMinDuration) {
		n, err := extractExact(input, writers, o, 0)
		return extractStats{mode: modeExact, exact: n}, err
	}

	stats, next, giveUp, err := extractKeyframes(input, writers, o)
	if err != nil || !giveUp {
		return stats, err
	}

	// Keyframes are too sparse: decode the rest straight through.
	n, err := extractExact(input, writers, o, next)
	stats.mode += "+" + modeExact
	stats.exact = n
	return stats, err
}

// extractExact decodes every frame from sample first onwards and samples with
// videorate. It returns the number of samples taken.
func extractExact(input string, writers []*sheetWriter, o extractOptions, first int) (int, error) {
	pipeline, sinks, err := buildPipeline(input, o, o.fps)
	if err != nil {
		return 0, err
	}
	defer pipeline.SetState(gst.StateNull)

	var count atomic.Int64
	for i, sink := range sinks {
		w := writers[i]
		// Runs on this branch's streaming thread for every frame.
		sink.ConnectNewSample(func(s gstapp.AppSink) gst.FlowReturn {
			sample := s.PullSample()
			if sample == nil {
				return gst.FlowEOS
			}
			if err := addSample(w, sample); err != nil {
				w.setErr(err)
				return gst.FlowError
			}
			if i == 0 {
				count.Add(1)
			}
			return gst.FlowOK
		})
		sink.SetObjectProperty("emit-signals", true)
	}

	// Pause first so the source frame rate is known, then seek to where
	// ffmpeg takes sample first; videorate continues every 1/fps from there.
	if err := pause(pipeline); err != nil {
		return 0, err
	}
	srcFrame := frameDuration(pipeline.GetByName("rate").GetStaticPad("sink").GetCurrentCaps())
	if start := sampleTime(first, o.fps, srcFrame); start > 0 {
		if !pipeline.SeekSimple(gst.FormatTime, gst.SeekFlagFlush|gst.SeekFlagAccurate, start.Nanoseconds()) {
			return 0, fmt.Errorf("seek to %s failed", start)
		}
	}

	if ret := pipeline.SetState(gst.StatePlaying); ret == gst.StateChangeFailure {
		if err := busError(pipeline); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("failed to set pipeline to playing")
	}

	bus := pipeline.GetBus()
	for {
		msg := bus.TimedPop(gst.ClockTimeNone)
		if msg == nil {
			continue
		}

		switch msg.Type() {
		case gst.MessageEOS:
			return int(count.Load()), nil

		case gst.MessageError:
			// An error from our own callbacks is more useful than GStreamer's generic flow error.
			for _, w := range writers {
				if err := w.firstErr(); err != nil {
					return 0, err
				}
			}
			debug, gstErr := msg.ParseError()
			return 0, fmt.Errorf("GStreamer: %w (%s)", gstErr, debug)
		}
	}
}

// extractKeyframes takes each sample from the nearest keyframe, falling back to
// an accurate seek when that keyframe is more than the allowed drift from the
// sample time or is not after the previous frame (sparse keyframes). In auto
// mode it gives up once too many samples need the fallback, and returns the
// next sample to take.
func extractKeyframes(input string, writers []*sheetWriter, o extractOptions) (stats extractStats, next int, giveUp bool, err error) {
	stats.mode = modeKeyframe

	pipeline, sinks, err := buildPipeline(input, o, 0)
	if err != nil {
		return stats, 0, false, err
	}
	defer pipeline.SetState(gst.StateNull)

	if err := pause(pipeline); err != nil {
		return stats, 0, false, err
	}

	// seek moves to t and returns one prerolled frame per profile, the frame's
	// stream time, and where the demuxer started the segment (for a key-unit
	// seek, the keyframe it snapped to). samples is nil at the end of the stream.
	seek := func(t time.Duration, flags gst.SeekFlags) (samples []*gst.Sample, pos, start time.Duration, err error) {
		if !pipeline.SeekSimple(gst.FormatTime, gst.SeekFlagFlush|flags, t.Nanoseconds()) {
			return nil, 0, 0, fmt.Errorf("seek to %s failed", t)
		}
		if err := pause(pipeline); err != nil {
			return nil, 0, 0, err
		}
		samples = make([]*gst.Sample, len(sinks))
		for i, sink := range sinks {
			if samples[i] = sink.TryPullPreroll(gst.ClockTime(30 * time.Second)); samples[i] == nil {
				if sink.IsEOS() {
					return nil, 0, 0, nil
				}
				if err := busError(pipeline); err != nil {
					return nil, 0, 0, err
				}
				return nil, 0, 0, fmt.Errorf("no frame at %s", t)
			}
		}
		seg := samples[0].GetSegment()
		pos = time.Duration(seg.ToStreamTime(gst.FormatTime, uint64(samples[0].GetBuffer().PTS())))
		start = time.Duration(seg.ToStreamTime(gst.FormatTime, seg.Start()))
		return samples, pos, start, nil
	}

	// The source frame rate is known once the pipeline has prerolled.
	first := sinks[0].TryPullPreroll(gst.ClockTime(30 * time.Second))
	if first == nil {
		if err := busError(pipeline); err != nil {
			return stats, 0, false, err
		}
		return stats, 0, false, fmt.Errorf("no video frames decoded from %q", input)
	}
	srcFrame := frameDuration(first.GetCaps())

	drift := min(o.maxDrift, time.Duration(0.5/o.fps*float64(time.Second)))
	last := time.Duration(-1)
	for k := 0; ; k++ {
		t := sampleTime(k, o.fps, srcFrame)
		if t >= o.duration {
			return stats, k, false, nil
		}

		if o.mode == modeAuto && k >= autoMinSamples &&
			float64(stats.repeated)/float64(k) > autoMaxRepeats {
			return stats, k, true, nil
		}

		samples, pos, key, err := seek(t, gst.SeekFlagKeyUnit|gst.SeekFlagSnapNearest)
		if err != nil {
			return stats, 0, false, err
		}
		var reason *int
		switch {
		case samples == nil:
			reason = &stats.drifted // no keyframe near the end; try the exact frame
		case key <= last+staleMargin:
			reason = &stats.repeated // same keyframe as the previous sample
		case (key - t).Abs() > drift:
			reason = &stats.drifted
		case (pos - key).Abs() > staleMargin:
			reason = &stats.stale
		}
		if reason == nil {
			stats.keyframes++
		} else {
			// Accurate seeks always move forward, so the decoder drops any
			// stale frames from earlier positions.
			if samples, pos, _, err = seek(t, gst.SeekFlagAccurate); err != nil {
				return stats, 0, false, err
			}
			if samples == nil {
				return stats, k, false, nil // past the last frame
			}
			*reason++
		}
		last = pos

		for i, w := range writers {
			if err := addSample(w, samples[i]); err != nil {
				return stats, 0, false, err
			}
		}
	}
}

// filmStrips writes large/<id>_lNN.jpg, medium/<id>_mNN.jpg and
// thumbnail/<id>_tNN.jpg sheets for input under outFldr, and returns the
// sheets per profile suffix.
func filmStrips(input, outFldr string, o extractOptions) (map[string][]string, extractStats, error) {
	// Rejects 0, negatives, NaN and +Inf (e.g. calculateFPS on a zero duration).
	if !(o.fps > 0) || math.IsInf(o.fps, 0) {
		return nil, extractStats{}, fmt.Errorf("fps must be a positive number, got %g", o.fps)
	}
	if o.threads < 0 {
		return nil, extractStats{}, fmt.Errorf("threads must not be negative, got %d", o.threads)
	}
	if o.mode != modeAuto && o.mode != modeExact && o.mode != modeKeyframe {
		return nil, extractStats{}, fmt.Errorf("mode must be %s, %s or %s, got %q", modeAuto, modeExact, modeKeyframe, o.mode)
	}
	if o.maxDrift <= 0 {
		return nil, extractStats{}, fmt.Errorf("max drift must be positive, got %s", o.maxDrift)
	}

	id := strings.TrimSuffix(filepath.Base(input), filepath.Ext(input))

	// Remove sheets from a previous run so a shorter video doesn't leave stale ones.
	encoders := make(chan struct{}, runtime.NumCPU())
	writers := make([]*sheetWriter, len(profiles))
	for i, p := range profiles {
		dir := filepath.Join(outFldr, p.folder)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, extractStats{}, fmt.Errorf("create %s: %w", dir, err)
		}
		prefix := filepath.Join(dir, id+"_"+p.suffix)
		if err := removeGlob(dir, filepath.Base(prefix)+"[0-9][0-9].jpg"); err != nil {
			return nil, extractStats{}, err
		}
		writers[i] = newSheetWriter(prefix, p, encoders)
	}

	stats, err := gstExtractFrames(input, writers, o)
	if err != nil {
		for _, w := range writers {
			w.wg.Wait() // don't leave encoders writing after we return
		}
		return nil, stats, err
	}

	sheets := make(map[string][]string, len(profiles))
	for i, p := range profiles {
		s, err := writers[i].finish()
		if err != nil {
			return nil, stats, err
		}
		if len(s) == 0 {
			return nil, stats, fmt.Errorf("no video frames decoded from %q", input)
		}
		sheets[p.suffix] = s
	}
	return sheets, stats, nil
}

func writeJPEG(path string, img image.Image, quality int) (err error) {
	output, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer func() {
		if cerr := output.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close %s: %w", path, cerr)
		}
	}()

	if err := jpeg.Encode(output, img, &jpeg.Options{Quality: quality}); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return nil
}

func main() {
	input := flag.String("input", "/Users/paullaidlaw/Workbench/Sample-Media/movie/The.Incomer.2026.1080p.ZF.webm", "video file")
	outFldr := flag.String("out", "gst-filmstrip", "folder for the sheets")
	deinterlace := flag.Bool("deinterlace", false, "deinterlace every frame before sampling (ffmpeg's [deinterlace])")
	threads := flag.Int("threads", 2, "software decoder threads, 0 = auto (ffmpeg's -threads)")
	mode := flag.String("mode", modeAuto, "frame extraction: auto, exact (decode every frame, matches ffmpeg) or keyframe (nearest keyframe per sample)")
	maxDrift := flag.Duration("max-drift", 2*time.Second, "keyframe mode: how far a keyframe may be from the sample time (at most half the sample interval)")
	flag.Parse()

	// runMain starts the Cocoa main-thread loop on macOS that GStreamer's GL
	// elements expect, then runs our code.
	runMain(func() {
		gst.Init()
		start := time.Now()

		duration, err := probeDuration(*input)
		if err != nil {
			log.Fatal(err)
		}
		fps, step := calculateFPS(duration)

		sheets, stats, err := filmStrips(*input, *outFldr, extractOptions{
			fps:         fps,
			duration:    time.Duration(duration * float64(time.Second)),
			mode:        *mode,
			maxDrift:    *maxDrift,
			deinterlace: *deinterlace,
			threads:     *threads,
		})
		if err != nil {
			log.Fatal(err)
		}

		fmt.Printf("input:    %s\n", *input)
		fmt.Printf("duration: %.3fs  fps: %.10f  (one frame every %.2fs, ~%.0f frames)\n", duration, fps, step, duration*fps)
		fmt.Printf("mode:     %s  (samples: keyframe=%d accurate=%d [drift=%d repeat=%d stale=%d] exact=%d)\n",
			stats.mode, stats.keyframes, stats.drifted+stats.repeated+stats.stale, stats.drifted, stats.repeated, stats.stale, stats.exact)
		fmt.Printf("sheets:   large=%d medium=%d thumbnail=%d in %s/\n", len(sheets["l"]), len(sheets["m"]), len(sheets["t"]), *outFldr)
		log.Printf("filmstrips made in %.3fs", time.Since(start).Seconds())
	})
}
