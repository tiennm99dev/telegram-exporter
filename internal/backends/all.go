//go:build !slim

// Package backends registers rclone storage backends by blank import.
//
// rclone resolves a remote by looking up its type in a registry that each
// backend populates from its own init(), so a backend that is not imported
// simply does not exist at runtime. The default build registers all of them, so
// any remote in the user's rclone.conf works and adding one later needs no
// rebuild. Cost is binary size: ~92 MB stripped, against ~50 MB for the slim
// build below.
package backends

import _ "github.com/rclone/rclone/backend/all"
