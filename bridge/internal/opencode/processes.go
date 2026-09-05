package opencode

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// processCacheTTL bounds how often the process table is read. The focused
// surface polls every few seconds and each poll would otherwise fork ps; which
// terminal is running an agent changes on the scale of minutes.
const processCacheTTL = 3 * time.Second

// TTYs reports the controlling terminals that currently have an opencode
// process, keyed by short tty name ("ttys007").
//
// This is what makes "is this surface running opencode?" a fact rather than a
// guess about its title: cmux reports each surface's tty (debug.terminals), and
// a tty with an opencode process on it is running opencode. A surface whose
// title happens to start with "OC | " but whose tty has no such process is not.
func (s *Store) TTYs() map[string]int {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	if s.procAt.After(time.Now().Add(-processCacheTTL)) {
		return s.procTTYs
	}
	s.procTTYs = scanTTYs()
	s.procAt = time.Now()
	return s.procTTYs
}

func scanTTYs() map[string]int {
	out, err := exec.Command("ps", "-o", "pid=,tty=,command=", "-A").Output()
	if err != nil {
		return nil
	}
	found := map[string]int{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		tty := fields[1]
		// Processes with no controlling terminal report "??" and can't belong
		// to a surface.
		if tty == "??" || tty == "?" {
			continue
		}
		if !isOpencodeCommand(fields[2]) {
			continue
		}
		found[NormalizeTTY(tty)] = pid
	}
	return found
}

// isOpencodeCommand reports whether an argv[0] is the opencode binary. Matching
// the executable's own name rather than searching the whole command line keeps
// `vim opencode.go`, or this bridge's own grep, from registering as an agent.
func isOpencodeCommand(argv0 string) bool {
	base := filepath.Base(strings.TrimSpace(argv0))
	return base == "opencode" || base == "opencode-tui"
}

// NormalizeTTY reduces the spellings of a terminal device to the short form ps
// prints: cmux reports "/dev/ttys007", ps prints "ttys007".
func NormalizeTTY(tty string) string {
	return strings.TrimPrefix(strings.TrimSpace(tty), "/dev/")
}
