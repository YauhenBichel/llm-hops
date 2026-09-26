// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package yllmgateway

import "os"

func inode(fi os.FileInfo) uint64 { return uint64(fi.ModTime().UnixNano()) }
