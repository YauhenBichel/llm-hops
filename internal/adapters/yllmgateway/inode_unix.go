// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package yllmgateway

import (
	"os"
	"syscall"
)

func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino) //nolint:unconvert // Ino is uint32 on some platforms
	}
	return 0
}
