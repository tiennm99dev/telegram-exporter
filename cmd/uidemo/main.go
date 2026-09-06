// Command uidemo renders the sync progress UI with fake data, so the layout can
// be checked without a Telegram session or a multi-hour transfer.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/iyear/tdl/core/tmedia"
	"github.com/rclone/rclone/fs"

	"github.com/tiennm99dev/telegram-exporter/internal/pipeline"
	"github.com/tiennm99dev/telegram-exporter/internal/report"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
	"github.com/tiennm99dev/telegram-exporter/internal/verify"
)

func item(id int, name string, size int64) tgsource.Item {
	return tgsource.Item{MessageID: id, Name: name, Media: &tmedia.Media{Size: size}}
}

func main() {
	files := []tgsource.Item{
		item(4242, "1234567890_4242_1000000000000000001.mp4", 96<<20),
		item(4243, "1234567890_4243_1000000000000000002.mp4", 1900<<20),
		item(4244, "1234567890_4244_short.jpg", 2<<20),
	}
	var total int64
	for _, f := range files {
		total += f.Size()
	}
	fmt.Fprintln(os.Stderr, "PikPak root 'mychannel' has 1438 GiB free")
	fmt.Fprintln(os.Stderr, "reading mychannel")
	fmt.Fprintln(os.Stderr, "  12,000 messages read in 4m31s")
	fmt.Fprintln(os.Stderr, "indexing PikPak root 'mychannel'")
	fmt.Fprintln(os.Stderr, "  11,406 objects listed in 2m10s")

	survey := verify.Report{Expected: 12000, Present: 11400, Bytes: 518 << 30}
	for i := range 2607 {
		survey.Absent = append(survey.Absent, 4242+i)
	}
	for i := range 6 {
		survey.Mismatched = append(survey.Mismatched,
			verify.Mismatch{MessageID: 14290 + i, Want: 2 << 30, Got: 221 << 20})
	}
	report.Survey(os.Stderr, survey)
	report.Plan(os.Stderr, report.PlanInfo{
		Files: 2613, Bytes: 79 << 30, Largest: 2 << 30, Budget: 40 << 30,
		Staging: "./staging", Threads: 4, Downloads: 2, Uploads: 2,
		Destination: "PikPak root 'mychannel'",
	})

	ev := report.Events(os.Stderr, 2613, 79<<30)
	if live, ok := ev.(*report.Live); ok {
		defer report.CaptureRcloneLog(context.Background(), live.LogWriter())()
	}
	st := pipeline.Stats{}
	for _, f := range files {
		ev.DownloadStart(f)
	}
	for step := range 30 {
		if step == 12 {
			fs.Errorf(nil, "1234567890_4245_1000000000000000003.jpg: Failed to copy: "+
				"can't verify the task is completed")
		}
		for _, f := range files {
			ev.DownloadBytes(f, f.Size()*int64(step+1)/30)
		}
		st.BytesDone += total / 30
		ev.Stats(st)
		time.Sleep(60 * time.Millisecond)
	}
	for i, f := range files {
		ev.DownloadDone(f, nil)
		ev.UploadStart(f)
		st.Done = i + 1
		ev.Stats(st)
		time.Sleep(400 * time.Millisecond)
		ev.UploadDone(f, nil)
	}
	ev.Finish(st)
}
