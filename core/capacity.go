package core

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/sandbox-hqr/agent/driver"
)

// probeCapacity probes local vCPU/RAM/disk at startup (draft/
// micro-machine.md §2). GPU detection isn't implemented — it needs
// platform-specific tooling (nvidia-smi parsing, etc.) out of scope for
// the Mock-driver MVP; GPUs always reports 0 here.
func probeCapacity() driver.Resources {
	return driver.Resources{
		VCPUs:  runtime.NumCPU(),
		MemMB:  probeMemMB(),
		DiskGB: probeDiskGB(),
	}
}

func probeMemMB() int {
	switch runtime.GOOS {
	case "linux":
		if kb, ok := readProcMeminfoTotalKB(); ok {
			return kb / 1024
		}
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			if bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
				return int(bytes / 1024 / 1024)
			}
		}
	}
	return 8192 // conservative fallback default
}

func readProcMeminfoTotalKB() (int, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, false
		}
		return kb, true
	}
	return 0, false
}

func probeDiskGB() int {
	// Best-effort via `df` rather than a syscall.Statfs dependency — good
	// enough for the Mock-driver MVP; real capacity accounting can be
	// tightened when a real VMM driver lands.
	out, err := exec.Command("df", "-Pk", "/").Output()
	if err != nil {
		return 100 // conservative fallback default
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 100
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 2 {
		return 100
	}
	kb, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 100
	}
	return int(kb / 1024 / 1024)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

// splice pipes bytes bidirectionally until either side closes — mirrors
// gox-apps/libs/reverse-tunnel-broker/rtb's splice (same wire shape, both
// ends of the same tunnel).
func splice(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}
