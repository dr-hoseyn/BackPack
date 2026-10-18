package naive

import (
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// Resources samples the current helper tree. RSS counts shared pages in each
// process; CPU is cumulative seconds for this process tree, not utilization.
// Never include command lines, generated configuration or environment values.
func (h *Helper) Resources() (rss uint64, cpu float64, measured bool) {
	pid := h.pid.Load()
	rss, cpu, measured = h.resources.read(pid, time.Now(), func() (uint64, float64, bool) {
		return sampleHelperResources(pid)
	})
	if pid != h.pid.Load() {
		return 0, 0, false
	}
	return
}

func sampleHelperResources(pid int32) (rss uint64, cpu float64, measured bool) {
	if pid <= 0 {
		return
	}
	root, err := process.NewProcess(pid)
	if err != nil {
		return
	}
	queue := []*process.Process{root}
	seen := map[int32]bool{}
	for len(queue) > 0 && len(seen) < 64 {
		p := queue[0]
		queue = queue[1:]
		if seen[p.Pid] {
			continue
		}
		seen[p.Pid] = true
		memory, err := p.MemoryInfo()
		if err != nil {
			return 0, 0, false
		}
		times, err := p.Times()
		if err != nil {
			return 0, 0, false
		}
		rss += memory.RSS
		cpu += times.User + times.System
		children, err := p.Children()
		if err != nil {
			return 0, 0, false
		}
		queue = append(queue, children...)
	}
	return rss, cpu, len(queue) == 0
}
