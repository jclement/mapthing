package server

import (
	"bytes"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// procSampler measures memory and CPU of process groups from /proc (Linux; zeros elsewhere).
// A supervised child is started in its own process group, so a group is the child and
// everything it started (the renderer's group holds the shell, Node and Xvfb). CPU needs two
// readings: each call compares with the previous one, taken at least a second earlier.
type procSampler struct {
	mu     sync.Mutex
	at     time.Time
	ticks  map[int]uint64 // process group → CPU ticks (user + system) at the last reading
	usage  map[int]procUsage
	self   procUsage // this process alone
	selfT  uint64
	procFS string
	hz     float64
}

type procUsage struct {
	RSSBytes   int64
	CPUPercent float64 // of one core; -1 until there are two readings
	Processes  int
}

func newProcSampler() *procSampler {
	// USER_HZ is 100 on every Linux build Go supports.
	return &procSampler{procFS: "/proc", hz: 100, ticks: map[int]uint64{}, usage: map[int]procUsage{}}
}

// groups returns usage by process group id.
func (p *procSampler) groups() map[int]procUsage {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if !p.at.IsZero() && now.Sub(p.at) < time.Second {
		return p.usage
	}
	ents, err := os.ReadDir(p.procFS)
	if err != nil {
		return p.usage
	}
	page := int64(os.Getpagesize())
	ticks := map[int]uint64{}
	usage := map[int]procUsage{}
	for _, e := range ents {
		if !e.IsDir() || e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		b, err := os.ReadFile(p.procFS + "/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		pgrp, cpu, rssPages, ok := parseProcStat(b)
		if !ok {
			continue
		}
		u := usage[pgrp]
		u.RSSBytes += rssPages * page
		u.Processes++
		usage[pgrp] = u
		ticks[pgrp] += cpu
	}
	elapsed := now.Sub(p.at).Seconds()
	for g, u := range usage {
		u.CPUPercent = -1
		if prev, ok := p.ticks[g]; ok && !p.at.IsZero() && elapsed > 0 && ticks[g] >= prev {
			u.CPUPercent = float64(ticks[g]-prev) / p.hz / elapsed * 100
		}
		usage[g] = u
	}
	// This process on its own: its group also holds the tools a build runs.
	if b, err := os.ReadFile(p.procFS + "/self/stat"); err == nil {
		if _, cpu, rssPages, ok := parseProcStat(b); ok {
			u := procUsage{RSSBytes: rssPages * page, CPUPercent: -1, Processes: 1}
			if !p.at.IsZero() && elapsed > 0 && cpu >= p.selfT {
				u.CPUPercent = float64(cpu-p.selfT) / p.hz / elapsed * 100
			}
			p.self, p.selfT = u, cpu
		}
	}
	p.at, p.ticks, p.usage = now, ticks, usage
	return usage
}

// own returns this process's usage (after groups has taken a reading).
func (p *procSampler) own() procUsage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.self
}

// parseProcStat reads the fields of /proc/<pid>/stat that follow the command name (which may
// itself contain spaces and parentheses): process group, utime + stime, and rss in pages.
func parseProcStat(b []byte) (pgrp int, cpuTicks uint64, rssPages int64, ok bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0, 0, false
	}
	f := strings.Fields(string(b[i+1:]))
	// f[0] is field 3 (state); pgrp is field 5, utime 14, stime 15, rss 24.
	if len(f) < 22 {
		return 0, 0, 0, false
	}
	pgrp, err1 := strconv.Atoi(f[2])
	ut, err2 := strconv.ParseUint(f[11], 10, 64)
	st, err3 := strconv.ParseUint(f[12], 10, 64)
	rss, err4 := strconv.ParseInt(f[21], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return 0, 0, 0, false
	}
	return pgrp, ut + st, rss, true
}

// loadAvg reads the 1-minute load average (Linux); -1 elsewhere.
func loadAvg() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return -1
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return -1
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return -1
	}
	return v
}

// cpuCount is the number of CPUs this process may use.
func cpuCount() int { return runtime.NumCPU() }
