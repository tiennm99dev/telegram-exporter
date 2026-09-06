//go:build slim

// Package backends registers rclone storage backends by blank import.
//
// The slim build registers only pikpak, the remote this tool was written
// against, trading generality for roughly half the binary size. A remote of any
// other type fails at resolution time with rclone's "didn't find backend"
// error, so build without the tag if the destination might change.
package backends

import _ "github.com/rclone/rclone/backend/pikpak"
