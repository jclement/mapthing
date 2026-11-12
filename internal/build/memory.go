package build

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Memory sources; variables so tests can point them at fixtures.
var (
	meminfoPath     = "/proc/meminfo"
	cgroupV2Max     = "/sys/fs/cgroup/memory.max"
	cgroupV2Stat    = "/sys/fs/cgroup/memory.stat"
	cgroupV1Limit   = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
	cgroupV1Stat    = "/sys/fs/cgroup/memory/memory.stat"
	cgroupUnlimited = int64(1) << 60 // v1 reports ~2^63 for "no limit"
)

// MemBudget is the memory a build may plan with.
type MemBudget struct {
	HostAvailable int64  `json:"host_available"`         // MemAvailable from /proc/meminfo
	CgroupLimit   int64  `json:"cgroup_limit,omitempty"` // container limit, 0 = none
	CgroupAnon    int64  `json:"cgroup_anon,omitempty"`  // non-reclaimable memory already in use in the container
	Effective     int64  `json:"effective"`              // what sizing uses
	SwapFree      int64  `json:"swap_free,omitempty"`    // host swap, counted by the build guard only
	Source        string `json:"source"`                 // "host", "cgroup v2", "cgroup v1"
}

// memoryBudget returns min(host MemAvailable, container limit − memory the container already
// holds (valhalla, renderer, this process)). Page cache is not counted as used: it is reclaimed.
func memoryBudget() MemBudget {
	b := MemBudget{HostAvailable: readMeminfo("MemAvailable"), SwapFree: readMeminfo("SwapFree"), Source: "host"}
	b.Effective = b.HostAvailable
	if lim, ok := readLimit(cgroupV2Max); ok {
		b.CgroupLimit, b.Source = lim, "cgroup v2"
		b.CgroupAnon = readStat(cgroupV2Stat, "anon")
	} else if lim, ok := readLimit(cgroupV1Limit); ok {
		b.CgroupLimit, b.Source = lim, "cgroup v1"
		b.CgroupAnon = readStat(cgroupV1Stat, "total_rss")
	}
	if b.CgroupLimit > 0 {
		free := b.CgroupLimit - b.CgroupAnon
		if b.Effective == 0 || free < b.Effective {
			b.Effective = free
		} else {
			b.Source = "host"
		}
	}
	return b
}

// readLimit reads a cgroup limit file; "max" or a huge value means no limit.
func readLimit(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 || v >= cgroupUnlimited {
		return 0, false
	}
	return v, true
}

func readStat(path, key string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if ok && k == key {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

func readMeminfo(key string) int64 {
	f, err := os.Open(meminfoPath)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), key+":"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return kb * 1024
		}
	}
	return 0
}

// parseJavaSize parses a -Xmx style size (4g, 4096m, 512M, 1073741824) into bytes.
func parseJavaSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mult = 1 << 10
	case 'm', 'M':
		mult = 1 << 20
	case 'g', 'G':
		mult = 1 << 30
	case 't', 'T':
		mult = 1 << 40
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return v * mult, nil
}

// memoryNeed is the memory a build asks for before it starts: about 768 MiB plus twice its OSM
// input, at least 1 GiB and at most 2 GiB (Planetiler sizes itself to what there is and goes to
// disk when short; Valhalla's graph build is what can run out), times
// MAPTHING_MEMORY_GUARD_FACTOR. A factor of 0 turns the check off.
func memoryNeed(inputBytes int64) int64 {
	need := int64(768<<20) + 2*inputBytes
	need = max(need, 1<<30)
	need = min(need, 2<<30)
	return int64(float64(need) * memoryGuardFactor())
}

// memoryGuardFactor reads MAPTHING_MEMORY_GUARD_FACTOR (default 1; 0 turns the check off;
// invalid or negative → 1).
func memoryGuardFactor() float64 {
	if v := os.Getenv("MAPTHING_MEMORY_GUARD_FACTOR"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return 1
}

// guardMemory is what the build guard counts as available: the effective budget, plus free
// swap when the container has no limit of its own (a little swapping during a build is fine;
// Planetiler's sizing does not count swap).
func (b MemBudget) guardMemory() int64 {
	if b.CgroupLimit > 0 {
		return b.Effective
	}
	return b.Effective + b.SwapFree
}
