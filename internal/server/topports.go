package server

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	topPortsMu           sync.Mutex
	topPortsCache        = map[int][]int{}
	fallbackTopPortsOnce sync.Once
	fallbackTopPorts     []int
)

func getTop1000Ports() []int {
	return getTopNPorts(1000)
}

func getTopNPorts(n int) []int {
	if n <= 0 {
		return nil
	}
	if n > 65535 {
		n = 65535
	}
	topPortsMu.Lock()
	cached, ok := topPortsCache[n]
	topPortsMu.Unlock()
	if ok {
		out := make([]int, len(cached))
		copy(out, cached)
		return out
	}

	ports := loadTopPortsFromNmapServices(n)
	if len(ports) == 0 {
		ports = getFallbackTopPorts(n)
	}

	topPortsMu.Lock()
	topPortsCache[n] = ports
	topPortsMu.Unlock()

	out := make([]int, len(ports))
	copy(out, ports)
	return out
}

func getFallbackTopPorts(n int) []int {
	if n <= 0 {
		return nil
	}
	fallbackTopPortsOnce.Do(func() {
		fallbackTopPorts = parsePortSpec(defaultFallbackTopPortsSpec)
	})
	if len(fallbackTopPorts) == 0 {
		out := make([]int, n)
		for i := 1; i <= n; i++ {
			out[i-1] = i
		}
		return out
	}
	if n <= len(fallbackTopPorts) {
		out := make([]int, n)
		copy(out, fallbackTopPorts[:n])
		return out
	}

	out := make([]int, 0, n)
	out = append(out, fallbackTopPorts...)
	seen := make(map[int]struct{}, len(out))
	for _, p := range out {
		seen[p] = struct{}{}
	}
	for p := 1; p <= 65535 && len(out) < n; p++ {
		if _, ok := seen[p]; ok {
			continue
		}
		out = append(out, p)
	}
	return out
}

func parsePortSpec(spec string) []int {
	parts := strings.Split(spec, ",")
	out := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, raw := range parts {
		tok := strings.TrimSpace(raw)
		if tok == "" {
			continue
		}
		if strings.Contains(tok, "-") {
			bounds := strings.SplitN(tok, "-", 2)
			if len(bounds) != 2 {
				continue
			}
			start, errA := strconv.Atoi(strings.TrimSpace(bounds[0]))
			end, errB := strconv.Atoi(strings.TrimSpace(bounds[1]))
			if errA != nil || errB != nil || start <= 0 || end <= 0 || start > end || end > 65535 {
				continue
			}
			for p := start; p <= end; p++ {
				if _, ok := seen[p]; ok {
					continue
				}
				seen[p] = struct{}{}
				out = append(out, p)
			}
			continue
		}
		p, err := strconv.Atoi(tok)
		if err != nil || p <= 0 || p > 65535 {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func loadTopPortsFromNmapServices(limit int) []int {
	if limit <= 0 {
		return nil
	}
	path := os.Getenv("NMAP_SERVICES_PATH")
	if path == "" {
		path = "/usr/share/nmap/nmap-services"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	type rec struct {
		port int
		freq float64
	}
	rows := make([]rec, 0, limit*2)
	seen := make(map[int]struct{})

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		portProto := fields[1]
		pp := strings.Split(portProto, "/")
		if len(pp) != 2 || pp[1] != "tcp" {
			continue
		}
		p, err := strconv.Atoi(pp[0])
		if err != nil || p < 1 || p > 65535 {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		freq, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			continue
		}
		seen[p] = struct{}{}
		rows = append(rows, rec{port: p, freq: freq})
	}
	if len(rows) == 0 {
		return nil
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].freq == rows[j].freq {
			return rows[i].port < rows[j].port
		}
		return rows[i].freq > rows[j].freq
	})

	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]int, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.port)
	}
	return out
}
