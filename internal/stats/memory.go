package stats

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Memory is the process memory picture the dashboard shows: the resident
// set the kernel enforces systemd's MemoryMax against, the Go heap behind
// it, and the cgroup limit the unit was started with (0 when unknown).
type Memory struct {
	RSSBytes       int64  `json:"rssBytes"`
	HeapAllocBytes int64  `json:"heapAllocBytes"`
	HeapSysBytes   int64  `json:"heapSysBytes"`
	LimitBytes     int64  `json:"limitBytes"`
	Goroutines     int    `json:"goroutines"`
	GCCycles       uint32 `json:"gcCycles"`
}

// memorySample refreshes the cached memory picture. Reading /proc and
// runtime statistics is cheap but not free, and every dashboard stream
// asks for it; one sample per second serves them all.
func (s *Stats) memory() Memory {
	s.memMu.Lock()
	defer s.memMu.Unlock()
	if time.Since(s.memAt) < time.Second {
		return s.mem
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s.mem = Memory{
		RSSBytes:       residentBytes(ms.Sys),
		HeapAllocBytes: int64(ms.HeapAlloc),
		HeapSysBytes:   int64(ms.HeapSys),
		LimitBytes:     s.memLimit,
		Goroutines:     runtime.NumGoroutine(),
		GCCycles:       ms.NumGC,
	}
	s.memAt = time.Now()
	return s.mem
}

// residentBytes reads VmRSS from /proc/self/status, falling back to the Go
// runtime's Sys total on platforms without procfs.
func residentBytes(fallback uint64) int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return int64(fallback)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			break
		}
		return kb * 1024
	}
	return int64(fallback)
}

// readMemoryLimit returns the cgroup memory limit systemd's MemoryMax
// installed, or 0 when the process is not limited. It reads the process's
// own cgroup (v2 and v1 layouts) so a hardened unit's ceiling is visible on
// the dashboard instead of being guessed.
func readMemoryLimit() int64 {
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			// cgroup v2: 0::/system.slice/go-mod-proxy01.service
			rel, ok := strings.CutPrefix(line, "0::")
			if !ok {
				continue
			}
			if n := readLimitFile("/sys/fs/cgroup" + rel + "/memory.max"); n > 0 {
				return n
			}
		}
	}
	if n := readLimitFile("/sys/fs/cgroup/memory.max"); n > 0 {
		return n
	}
	return readLimitFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
}

func readLimitFile(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value := strings.TrimSpace(string(data))
	if value == "" || value == "max" {
		return 0
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	// cgroup v1 reports "unlimited" as a page-aligned near-max value.
	if n > 1<<60 {
		return 0
	}
	return n
}
