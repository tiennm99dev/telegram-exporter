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

// Live renders a run as a set of progress bars: one per leg, plus one for each
// file currently moving.
//
// The aggregate line it replaces could say how much was done but never what was
// happening — which files were in flight, whether a stall was a slow download or
// a slow upload, how long the rest would take. On a run measured in hours those
// are the only questions worth answering.
//
// Download and upload are counted separately because they run at different
// speeds and fail for different reasons. A single combined figure hides the one
// thing worth knowing when a run slows down: whether Telegram or the remote is
// the bottleneck. The two normally track each other a file or two apart; a
// widening gap is the remote falling behind, and staging filling up.
//
// Bars are for terminals only. A redirected run gets lineEvents instead, because
// this writes ANSI cursor movement continuously and a captured log of it is
// unreadable.
type Live struct {
	w       io.Writer
	p       *mpb.Progress
	dlTotal *mpb.Bar
	upTotal *mpb.Bar

	mu   sync.Mutex
	down map[int]*mpb.Bar
	up   map[int]*mpb.Bar
	// The leg counters are read by the total bars' decorators from mpb's render
	// goroutine, so they are guarded by the same lock as the maps.
	legs legTotals

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
	l.dlTotal = l.leg(bytes, 0, "  ↓ total", func() int { return l.legs.downCount() })
	l.upTotal = l.leg(bytes, 1, "  ↑ total", func() int { return l.legs.upCount() })
	return l
}

// leg builds one of the two whole-run bars.
func (l *Live) leg(bytes int64, priority int, label string, count func() int) *mpb.Bar {
	return l.p.New(bytes,
		mpb.BarStyle().Lbound("[").Filler("=").Tip(">").Padding(" ").Rbound("]"),
		mpb.BarPriority(priority),
		mpb.BarNoPop(),
		mpb.PrependDecorators(
			decor.Name(label+"  ", decor.WC{W: 11}),
			decor.Any(func(decor.Statistics) string {
				return fmt.Sprintf("%s/%s files", humanCount(count()), humanCount(l.files))
			}, decor.WC{W: 16}),
		),
		mpb.AppendDecorators(
			decor.CountersKibiByte("% .1f / % .1f", decor.WC{W: 20}),
			decor.AverageSpeed(decor.SizeB1024(0), " % .1f", decor.WC{W: 12}),
			// Wide enough for the three-digit hour counts a slow remote
			// produces; at W:10 the ETA ran into the speed beside it.
			decor.OnComplete(decor.AverageETA(decor.ET_STYLE_GO, decor.WC{W: 13}), ""),
		),
	)
}

// Bars are grouped by band: the two totals on top, then per-file downloads,
// then per-file uploads. Within a band they are ordered by when they started.
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

// Stats advances the download bar.
func (l *Live) Stats(s pipeline.Stats) {
	l.legs.setDownload(s.Done+s.Failed, s.BytesDone)
	// The bar's own counters, speed and ETA all derive from this, so it is what
	// makes the total move rather than sitting at zero.
	l.dlTotal.SetCurrent(s.BytesDone)
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

func (l *Live) UploadDone(it tgsource.Item, err error) {
	l.mu.Lock()
	bar := l.up[it.MessageID]
	delete(l.up, it.MessageID)
	l.mu.Unlock()
	if bar != nil {
		bar.Abort(true)
	}
	if err != nil {
		return // nothing reached the remote, so the upload total does not move
	}
	l.upTotal.SetCurrent(l.legs.addUpload(it.Size()))
}

// Retry prints above the bars rather than through them, so the pass that is
// about to start is announced without shredding the display.
func (l *Live) Retry(attempt, files int, wait time.Duration) {
	fmt.Fprintf(l.p, "  retrying %s file(s) in %s — attempt %d\n",
		humanCount(files), wait.Round(time.Second), attempt)
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

	l.dlTotal.Abort(true)
	l.upTotal.Abort(true)
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
