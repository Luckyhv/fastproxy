package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// perStreamBudget is the RAM reserved per concurrent stream: the copy buffer plus
// ~1.25 MiB of net/http buffers, TLS state and the upstream side. Measured Aug
// 2026 at 2.2–2.35 MiB/viewer with a 1 MiB buffer.
var perStreamBudget = uint64(streamBufferSize + 1280*1024)

// capacityFor derives the in-flight cap from RAM alone: half of memory on
// streams, the rest for the OS, runtime and spikes. No CPU term — the work is
// I/O, and a per-core cap only turned real traffic into 503s on small VPSes.
// 0 = unlimited (RAM undetected, e.g. a macOS dev box).
func capacityFor(mem uint64) int {
	if mem == 0 {
		return 0
	}
	return max(64, min(int(mem/2/perStreamBudget), 200000))
}

// Called once before listening. Recalculate on every process start so moving
// between VPS/container sizes needs no hand-edited capacity settings.
func autoTune() {
	cpus := effectiveCPUs()
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(cpus)
	}
	cpus = min(cpus, runtime.GOMAXPROCS(0))
	mem := effectiveMemoryBytes()
	tuneConcurrency(mem)
	tuneTransports(mem)
	if mem > 0 && os.Getenv("GOMEMLIMIT") == "" {
		// Soft Go-runtime target; excludes kernel socket buffers and other processes.
		limit := int64(min(mem/10*7, uint64(1<<63-1)))
		debug.SetMemoryLimit(limit)
		log.Printf("autotune: Go memory target=%s (soft limit)", humanBytes(uint64(limit)))
	}
	log.Printf("autotune: cpus=%d (GOMAXPROCS=%d), ram=%s", cpus, runtime.GOMAXPROCS(0), humanBytes(mem))
}

func tuneConcurrency(mem uint64) {
	capacity := capacityFor(mem)
	if raw := getenv("MAX_CONCURRENT", ""); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			capacity = n
		}
	}
	if capacity == 0 {
		inFlight = nil
	} else {
		inFlight = make(chan struct{}, capacity)
	}
	log.Printf("autotune: active requests=%d (0=unlimited)", capacity)
}

// effectiveCPUs returns the smaller of the host core count and any cgroup CPU
// quota — so a container limited to 2 cores on a 64-core host reports 2.
func effectiveCPUs() int {
	n := runtime.NumCPU()
	if q := cgroupCPUQuota(); q > 0 && q < n {
		n = q
	}
	if n < 1 {
		n = 1
	}
	return n
}

// cgroupCPUQuota returns ceil(quota/period) cores, or 0 if unlimited/unknown.
func cgroupCPUQuota() int {
	// cgroup v2: "/sys/fs/cgroup/cpu.max" = "<quota> <period>" or "max <period>"
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		f := strings.Fields(strings.TrimSpace(string(b)))
		if len(f) == 2 && f[0] != "max" {
			q, _ := strconv.Atoi(f[0])
			p, _ := strconv.Atoi(f[1])
			if q > 0 && p > 0 {
				return (q + p - 1) / p // round up
			}
		}
		return 0
	}
	// cgroup v1: separate quota + period files.
	q := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	p := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if q > 0 && p > 0 {
		return (q + p - 1) / p
	}
	return 0
}

// effectiveMemoryBytes returns the smaller of physical RAM and any cgroup memory
// limit, or 0 if neither could be read (non-Linux dev machines).
func effectiveMemoryBytes() uint64 {
	var mem uint64

	// Physical RAM from /proc/meminfo (Linux only).
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				if f := strings.Fields(line); len(f) >= 2 {
					if kb, err := strconv.ParseUint(f[1], 10, 64); err == nil {
						mem = kb * 1024 // meminfo is in kB
					}
				}
				break
			}
		}
	}

	// Container memory limit — take the min so a limited container respects it.
	if lim := cgroupMemLimit(); lim > 0 && (mem == 0 || lim < mem) {
		mem = lim
	}
	return mem
}

func cgroupMemLimit() uint64 {
	// cgroup v2
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "max" {
			if v, err := strconv.ParseUint(s, 10, 64); err == nil {
				return v
			}
		}
		return 0
	}
	// cgroup v1: an "unlimited" limit is a huge sentinel (~max int64), so ignore
	// anything implausibly large.
	if b, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil && v < (1<<62) {
			return v
		}
	}
	return 0
}

func readIntFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func humanBytes(b uint64) string {
	if b == 0 {
		return "unknown"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
