package scanner

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"multiscan/internal/protocol"
)

// Config controls scan behavior for each endpoint.
type Config struct {
	Timeout     time.Duration
	Concurrency int
}

// ScanRange scans every (ip,port) pair in the provided inclusive ranges.
func ScanRange(startIP, endIP string, startPort, endPort int, cfg Config) ([]protocol.ScanResult, error) {
	ports := make([]int, 0, endPort-startPort+1)
	for p := startPort; p <= endPort; p++ {
		ports = append(ports, p)
	}
	return ScanPorts(startIP, endIP, ports, cfg)
}

// ScanPorts scans every (ip,port) pair for the provided port list.
func ScanPorts(startIP, endIP string, ports []int, cfg Config) ([]protocol.ScanResult, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 700 * time.Millisecond
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 128
	}

	ips, err := ipRange(startIP, endIP)
	if err != nil {
		return nil, err
	}
	ports, err = normalizePorts(ports)
	if err != nil {
		return nil, err
	}

	type task struct {
		ip   string
		port int
	}

	tasks := make(chan task)
	results := make(chan protocol.ScanResult)
	var wg sync.WaitGroup

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range tasks {
				endpoint := net.JoinHostPort(t.ip, fmt.Sprintf("%d", t.port))
				conn, dialErr := net.DialTimeout("tcp", endpoint, cfg.Timeout)
				if dialErr != nil {
					results <- protocol.ScanResult{IP: t.ip, Port: t.port, Open: false, Err: dialErr.Error()}
					continue
				}
				_ = conn.Close()
				results <- protocol.ScanResult{IP: t.ip, Port: t.port, Open: true}
			}
		}()
	}

	go func() {
		for _, ip := range ips {
			for _, port := range ports {
				tasks <- task{ip: ip, port: port}
			}
		}
		close(tasks)
		wg.Wait()
		close(results)
	}()

	out := make([]protocol.ScanResult, 0, len(ips)*len(ports))
	for r := range results {
		out = append(out, r)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].IP == out[j].IP {
			return out[i].Port < out[j].Port
		}
		return out[i].IP < out[j].IP
	})

	return out, nil
}

func normalizePorts(ports []int) ([]int, error) {
	if len(ports) == 0 {
		return nil, fmt.Errorf("no ports provided")
	}
	out := make([]int, 0, len(ports))
	seen := make(map[int]struct{}, len(ports))
	for _, p := range ports {
		if p <= 0 || p > 65535 {
			return nil, fmt.Errorf("invalid port %d", p)
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	slices.Sort(out)
	return out, nil
}

func ipRange(start, end string) ([]string, error) {
	startAddr, err := netip.ParseAddr(start)
	if err != nil {
		return nil, fmt.Errorf("invalid start IP: %w", err)
	}
	endAddr, err := netip.ParseAddr(end)
	if err != nil {
		return nil, fmt.Errorf("invalid end IP: %w", err)
	}
	if startAddr.BitLen() != 32 || endAddr.BitLen() != 32 {
		return nil, fmt.Errorf("only IPv4 addresses are supported")
	}

	startNum := ipv4ToUint32(startAddr)
	endNum := ipv4ToUint32(endAddr)
	if startNum > endNum {
		return nil, fmt.Errorf("start IP must be <= end IP")
	}

	ips := make([]string, 0, int(endNum-startNum)+1)
	for i := startNum; i <= endNum; i++ {
		ips = append(ips, uint32ToIPv4(i).String())
	}
	return ips, nil
}

func ipv4ToUint32(addr netip.Addr) uint32 {
	b := addr.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func uint32ToIPv4(i uint32) netip.Addr {
	b := [4]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}
	return netip.AddrFrom4(b)
}
