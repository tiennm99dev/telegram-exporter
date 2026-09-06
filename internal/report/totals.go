package report

import "sync"

// legTotals counts what each half of the run has moved.
//
// The two legs are tracked separately because they report differently: the
// downloader emits byte-level progress, while an upload is one blocking rclone
// call that only reports on completion. Keeping the counters here rather than in
// each renderer means the terminal and the log agree on the same numbers.
type legTotals struct {
	mu      sync.Mutex
	dlFiles int
	dlBytes int64
	upFiles int
	upBytes int64
}

// setDownload records the downloader's running totals.
func (t *legTotals) setDownload(files int, bytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dlFiles, t.dlBytes = files, bytes
}

// addUpload records one file that reached the remote and returns the new total.
// A whole file at a time is all that is knowable: rclone's MoveFile does not
// report progress within a transfer.
func (t *legTotals) addUpload(size int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.upFiles++
	t.upBytes += size
	return t.upBytes
}

func (t *legTotals) downCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dlFiles
}

func (t *legTotals) upCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.upFiles
}

// snapshot returns both legs at once, so a line rendered from it is consistent.
func (t *legTotals) snapshot() (dlFiles int, dlBytes int64, upFiles int, upBytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dlFiles, t.dlBytes, t.upFiles, t.upBytes
}
