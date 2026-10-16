//go:build linux

package lock

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// findLockHolder inspects /proc/locks to locate the PID and process command holding the flock on lockPath.
func findLockHolder(lockPath string) (int, string) {
	st, err := os.Stat(lockPath)
	if err != nil {
		return 0, ""
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, ""
	}

	dev := sys.Dev
	major := (dev >> 8) & 0xfff
	minor := (dev & 0xff) | ((dev >> 12) & ^uint64(0xff))
	targetDevIno := fmt.Sprintf("%02x:%02x:%d", major, minor, sys.Ino)

	f, err := os.Open("/proc/locks")
	if err != nil {
		return 0, ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		// Expected /proc/locks format:
		// 1: FLOCK  ADVISORY  WRITE 1756618 103:02:13417268 0 EOF
		// fields[4] is the holding PID, fields[5] is major:minor:inode
		if len(fields) >= 6 && fields[5] == targetDevIno {
			pid, err := strconv.Atoi(fields[4])
			if err == nil && pid > 0 {
				commBytes, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
				comm := strings.TrimSpace(string(commBytes))
				return pid, comm
			}
		}
	}
	return 0, ""
}
