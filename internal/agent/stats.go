// Package agent collects system stats and manages services on the desktop.
package agent

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GPU is a snapshot of one NVIDIA GPU from nvidia-smi.
type GPU struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	UtilPct    int    `json:"util_pct"`
	MemUsedMB  uint64 `json:"mem_used_mb"`
	MemTotalMB uint64 `json:"mem_total_mb"`
	TempC      int    `json:"temp_c"`
}

// SystemStats is a point-in-time snapshot of the host.
type SystemStats struct {
	UptimeSec  float64 `json:"uptime_sec"`
	Load1      float64 `json:"load1"`
	Load5      float64 `json:"load5"`
	Load15     float64 `json:"load15"`
	MemTotalMB uint64  `json:"mem_total_mb"`
	MemUsedMB  uint64  `json:"mem_used_mb"`
	GPUs       []GPU   `json:"gpus,omitempty"`
}

// Collect samples /proc and nvidia-smi into a SystemStats snapshot.
// Failures degrade to zero/empty values.
func Collect(ctx context.Context) SystemStats {
	var s SystemStats
	s.UptimeSec = uptime()
	s.Load1, s.Load5, s.Load15 = loadAvg()
	s.MemTotalMB, s.MemUsedMB = memInfo()
	s.GPUs = gpus(ctx)
	return s
}

func uptime() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func loadAvg() (float64, float64, float64) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	f := strings.Fields(string(data))
	if len(f) < 3 {
		return 0, 0, 0
	}
	a, _ := strconv.ParseFloat(f[0], 64)
	b, _ := strconv.ParseFloat(f[1], 64)
	c, _ := strconv.ParseFloat(f[2], 64)
	return a, b, c
}

func memInfo() (uint64, uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var total, avail uint64
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = kBField(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			avail = kBField(line)
		}
	}
	return total / 1024, (total - avail) / 1024
}

func kBField(line string) uint64 {
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(f[1], 10, 64)
	return v
}

const gpuQuery = "index,name,utilization.gpu,memory.used,memory.total,temperature.gpu"

func gpus(ctx context.Context) []GPU {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu="+gpuQuery, "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil // no NVIDIA GPU/driver — not an error condition
	}
	var gpus []GPU
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 6 {
			continue
		}
		g := GPU{Name: strings.TrimSpace(f[1])}
		g.Index, _ = strconv.Atoi(strings.TrimSpace(f[0]))
		g.UtilPct, _ = strconv.Atoi(strings.TrimSpace(f[2]))
		g.MemUsedMB, _ = strconv.ParseUint(strings.TrimSpace(f[3]), 10, 64)
		g.MemTotalMB, _ = strconv.ParseUint(strings.TrimSpace(f[4]), 10, 64)
		g.TempC, _ = strconv.Atoi(strings.TrimSpace(f[5]))
		gpus = append(gpus, g)
	}
	return gpus
}
