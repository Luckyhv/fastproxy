package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// perStreamBudget is the RAM we conservatively reserve per concurrent stream when
// auto-deriving MAX_CONCURRENT: the copy buffer (see streamBufSize), the
// per-connection transport buffers on both sides, and TLS state.
//
// This MUST track streamBufSize. With the 512 KiB copy buffer and 64/8 KiB
// upstream transport buffers, 2 MiB per viewer is a conservative admission
// budget with headroom for TLS/socket/runtime overhead.
//
// If streamBufSize ever drops to 64 KiB, this can drop to ~768 KiB (measured
// 622 KiB/viewer), roughly tripling the viewers a given box will accept.
const perStreamBudget = 2048 * 1024

// autoTune inspects the machine/container and picks sane runtime defaults,
// logging what it found. Explicit env vars (GOMAXPROCS, MAX_CONCURRENT) always
// win — auto-tuning only fills in what you didn't set.
func autoTune() {
	cpus := effectiveCPUs()
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(cpus) // pin to the *effective* core count
	}

	mem := effectiveMemoryBytes() // 0 = couldn't detect (e.g. macOS dev box)

	switch {
	case os.Getenv("MAX_CONCURRENT") != "":
		// Operator set it explicitly — honor it (0 = unlimited).
		if n := atoiDefault(os.Getenv("MAX_CONCURRENT"), 0); n > 0 {
			inFlight = make(chan struct{}, n)
			log.Printf("autotune: MAX_CONCURRENT=%d (explicit)", n)
		} else {
			log.Printf("autotune: MAX_CONCURRENT=unlimited (explicit)")
		}
	case mem > 0:
		// Derive a cap from RAM: spend ~half of memory on stream buffers, leave
		// the rest for the OS, the Go runtime, and traffic spikes. Clamp to a
		// sane range so tiny or huge boxes still get a reasonable number.
		n := int((mem / 2) / perStreamBudget)
		if n < 100 {
			n = 100
		}
		if n > 200000 {
			n = 200000
		}
		inFlight = make(chan struct{}, n)
		log.Printf("autotune: MAX_CONCURRENT=%d (derived from %s RAM)", n, humanBytes(mem))
	default:
		log.Printf("autotune: MAX_CONCURRENT=unlimited (RAM undetected; set it explicitly to cap)")
	}

	log.Printf("autotune: cpus=%d (GOMAXPROCS=%d), ram=%s", cpus, runtime.GOMAXPROCS(0), humanBytes(mem))
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
