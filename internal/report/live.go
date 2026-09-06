package report

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"

	"github.com/tiennm99dev/telegram-exporter/internal/pipeline"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// nameWidth is how much of a filename a per-file bar shows. Telegram names run
// to 100+ characters and the bar has to fit beside them.
const nameWidth = 34

// Live renders a run as a set of progress bars: one overall, plus one for each
// file currently moving.
//
// The aggregate line it replaces could say how much was done but never what was
// happening — which files were in flight, whether a stall was a slow download or
// a slow upload, how long the rest would take. On a run measured in hours those
// are the only questions worth answering.
//
// Bars are for terminals only. A redirected run gets lineEvents instead, because
// this writes ANSI cursor movement continuously and a captured log of it is
// unreadable.
type Live struct {
	w     io.Writer
	p     *mpb.Progress
	total *mpb.Bar

	mu   sync.Mutex
	down map[int]*mpb.Bar
	up   map[int]*mpb.Bar
	// done is read by the overall bar's decorator from mpb's render goroutine,
	// so it is guarded by the same lock as the maps.
	done int

	// seq gives each per-file bar a distinct, increasing priority so bars keep
	// their position between frames. Sharing one priority lets mpb reorder them
	// on every redraw, which makes a steady transfer look like it is thrashing.
	seq   int
	files int
	start time.Time
}

// LogWriter returns a writer whose lines are printed above the bars rather than
// through them.
//
// rclone logs straight to stderr on its own schedule, so without this its error
// lines land in the middle of a redraw and shred the display — which is exactly
// what a run full of pikpak commit failures looked like.
func (l *Live) LogWriter() io.Writer { return l.p }

// NewLive builds a bar renderer over w for a run of the given size.
func NewLive(w io.Writer, files int, bytes int64) *Live {
	p := mpb.New(
		mpb.WithOutput(w),
		mpb.WithWidth(28),
		mpb.WithRefreshRate(120*time.Millisecond),
	)
	l := &Live{
		w:     w,
		p:     p,
		down:  make(map[int]*mpb.Bar),
		up:    make(map[int]*mpb.Bar),
		files: files,
		start: time.Now(),
	}
	l.total = p.New(bytes,
		mpb.BarStyle().Lbound("[").Filler("=").Tip(">").Padding(" ").Rbound("]"),
		mpb.BarPriority(0),
		mpb.BarNoPop(),
		mpb.PrependDecorators(
			decor.Name("  total  ", decor.WC{W: 9}),
			decor.Any(func(decor.Statistics) string { return l.counts() }, decor.WC{W: 16}),
		),
		mpb.AppendDecorators(
			decor.CountersKibiByte("% .1f / % .1f", decor.WC{W: 20}),
			decor.AverageSpeed(decor.SizeB1024(0), " % .1f", decor.WC{W: 12}),
			// Wide enough for the three-digit hour counts a slow remote
			// produces; at W:10 the ETA ran into the speed beside it.
			decor.OnComplete(decor.AverageETA(decor.ET_STYLE_GO, decor.WC{W: 13}), ""),
		),
	)
	return l
}

// Bars are grouped by band: the total on top, then downloads, then uploads.
// Within a band they are ordered by when they started.
const (
	downloadBand = 1 << 20
	uploadBand   = 1 << 21
)

// next allocates the priority for a new bar in the given band.
func (l *Live) next(band int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	return band + l.seq
}

func (l *Live) counts() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprintf("%s/%s files", humanCount(l.done), humanCount(l.files))
}

// Stats advances the overall bar.
func (l *Live) Stats(s pipeline.Stats) {
	l.mu.Lock()
	l.done = s.Done + s.Failed
	l.mu.Unlock()
	// The bar's own counters, speed and ETA all derive from this, so it is what
	// makes the totals move rather than sitting at zero.
	l.total.SetCurrent(s.BytesDone)
}

func (l *Live) DownloadStart(it tgsource.Item) {
	bar := l.p.New(it.Size(),
		mpb.BarStyle().Lbound("[").Filler("=").Tip(">").Padding(" ").Rbound("]"),
		mpb.BarRemoveOnComplete(),
		mpb.BarPriority(l.next(downloadBand)),
		mpb.PrependDecorators(
			decor.Name("  ↓ "),
			decor.Name(short(it.Name), decor.WC{W: nameWidth + 2, C: decor.DindentRight}),
		),
		mpb.AppendDecorators(
			decor.CountersKibiByte("% .1f / % .1f", decor.WC{W: 20}),
			decor.AverageSpeed(decor.SizeB1024(0), " % .1f", decor.WC{W: 12}),
		),
	)
	l.mu.Lock()
	l.down[it.MessageID] = bar
	l.mu.Unlock()
}

func (l *Live) DownloadBytes(it tgsource.Item, done int64) {
	l.mu.Lock()
	bar := l.down[it.MessageID]
	l.mu.Unlock()
	if bar != nil {
		bar.SetCurrent(done)
	}
}

func (l *Live) DownloadDone(it tgsource.Item, err error) {
	l.mu.Lock()
	bar := l.down[it.MessageID]
	delete(l.down, it.MessageID)
	l.mu.Unlock()
	if bar == nil {
		return
	}
	// Aborted rather than completed on failure, so a bar for a file that never
	// arrived does not linger at 100%.
	if err != nil {
		bar.Abort(true)
		return
	}
	bar.SetCurrent(it.Size())
}

func (l *Live) UploadStart(it tgsource.Item) {
	// No byte-level callbacks exist for the upload leg — rclone's MoveFile is a
	// single blocking call — so this is a spinner, not a bar. Showing which file
	// is uploading is the point: a run that looks stalled is usually waiting on
	// one large object, and until now nothing said so.
	bar := l.p.New(0, mpb.SpinnerStyle().PositionLeft(),
		mpb.BarRemoveOnComplete(),
		mpb.BarPriority(l.next(uploadBand)),
		mpb.PrependDecorators(
			decor.Name("  ↑ "),
			decor.Name(short(it.Name), decor.WC{W: nameWidth + 2, C: decor.DindentRight}),
		),
		mpb.AppendDecorators(
			decor.Any(func(decor.Statistics) string {
				return fmt.Sprintf("uploading %s", humanBytes(it.Size()))
			}, decor.WC{W: 24}),
		),
	)
	l.mu.Lock()
	l.up[it.MessageID] = bar
	l.mu.Unlock()
}

func (l *Live) UploadDone(it tgsource.Item, _ error) {
	l.mu.Lock()
	bar := l.up[it.MessageID]
	delete(l.up, it.MessageID)
	l.mu.Unlock()
	if bar != nil {
		bar.Abort(true)
	}
}

// Finish drains the bars and prints the closing summary.
func (l *Live) Finish(s pipeline.Stats) {
	l.mu.Lock()
	for _, b := range l.down {
		b.Abort(true)
	}
	for _, b := range l.up {
		b.Abort(true)
	}
	clear(l.down)
	clear(l.up)
	l.mu.Unlock()

	l.total.Abort(true)
	l.p.Wait()
	writeSummary(l.w, s, time.Since(l.start))
}

// short trims a filename to fit beside a bar, keeping the end — the extension
// and the distinguishing digits are there, while the shared dialog-id prefix is
// not.
func short(name string) string {
	r := []rune(name)
	if len(r) <= nameWidth {
		return name
	}
	return "…" + string(r[len(r)-nameWidth+1:])
}
