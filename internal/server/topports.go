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
	topPortsMu    sync.Mutex
	topPortsCache = map[int][]int{}
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
		ports = make([]int, n)
		for i := 1; i <= n; i++ {
			ports[i-1] = i
		}
	}

	topPortsMu.Lock()
	topPortsCache[n] = ports
	topPortsMu.Unlock()

	out := make([]int, len(ports))
	copy(out, ports)
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
