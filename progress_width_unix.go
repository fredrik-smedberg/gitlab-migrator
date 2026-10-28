//go:build !windows

package main

import "golang.org/x/sys/unix"

func terminalWidthFd(fd int) int {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws == nil {
		return 0
	}
	return int(ws.Col)
}
