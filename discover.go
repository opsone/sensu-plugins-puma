// Discovery of the Puma instances running on the host, from their process title.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, the unit of the process start time in /proc/<pid>/stat.
// It is 100 on every mainstream Linux architecture (no sysconf without CGO).
const clockTicks = 100

var (
	// puma 6.4.2 (unix:///var/www/myapp_prod/shared/tmp/sockets/puma.sock) [myapp_prod]
	masterTitle = regexp.MustCompile(`^puma \S+ \(.*\) \[(\S+)\]$`)
	// puma: cluster worker 0: 12345 [myapp_prod]
	workerTitle = regexp.MustCompile(`^puma: cluster worker (\d+): (\d+) \[(\S+)\]$`)
)

// Process is a Puma process seen in /proc.
type Process struct {
	PID       int
	StartedAt time.Time
	RSS       uint64 // bytes
}

// Worker is a Puma cluster worker process.
type Worker struct {
	Process
	Index     int
	MasterPID int
}

// Instance groups the processes sharing the same Puma tag (--tag).
type Instance struct {
	Tag     string
	App     string
	Env     string
	Masters []Process
	Workers []Worker
}

// SplitTag splits a "{app}_{env}" tag on its last underscore: my_app_prod → my_app, prod.
func SplitTag(tag string) (app, env string) {
	i := strings.LastIndex(tag, "_")
	if i <= 0 || i == len(tag)-1 {
		return tag, ""
	}
	return tag[:i], tag[i+1:]
}

// Discover scans procRoot (normally /proc) for tagged Puma processes, grouped by tag and sorted.
// Processes vanishing during the scan are ignored.
func Discover(procRoot string) ([]*Instance, error) {
	bootTime, err := readBootTime(procRoot)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}

	byTag := map[string]*Instance{}
	instance := func(tag string) *Instance {
		if byTag[tag] == nil {
			app, env := SplitTag(tag)
			byTag[tag] = &Instance{Tag: tag, App: app, Env: env}
		}
		return byTag[tag]
	}

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		title, err := readTitle(procRoot, pid)
		if err != nil {
			continue
		}
		if m := masterTitle.FindStringSubmatch(title); m != nil {
			p, err := readProcess(procRoot, pid, bootTime)
			if err != nil {
				continue
			}
			inst := instance(m[1])
			inst.Masters = append(inst.Masters, p)
		} else if m := workerTitle.FindStringSubmatch(title); m != nil {
			p, err := readProcess(procRoot, pid, bootTime)
			if err != nil {
				continue
			}
			index, _ := strconv.Atoi(m[1])
			masterPID, _ := strconv.Atoi(m[2])
			inst := instance(m[3])
			inst.Workers = append(inst.Workers, Worker{Process: p, Index: index, MasterPID: masterPID})
		}
	}

	instances := make([]*Instance, 0, len(byTag))
	for _, inst := range byTag {
		sort.Slice(inst.Masters, func(i, j int) bool { return inst.Masters[i].PID < inst.Masters[j].PID })
		sort.Slice(inst.Workers, func(i, j int) bool { return inst.Workers[i].Index < inst.Workers[j].Index })
		instances = append(instances, inst)
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].Tag < instances[j].Tag })
	return instances, nil
}

// readTitle returns the process title as set by Puma (argv rewritten, NUL separated and padded).
func readTitle(procRoot string, pid int) (string, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return "", err
	}
	b = bytes.TrimRight(b, "\x00 ")
	return string(bytes.ReplaceAll(b, []byte{0}, []byte{' '})), nil
}

func readProcess(procRoot string, pid int, bootTime time.Time) (Process, error) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))

	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return Process{}, err
	}
	// The command name (field 2) may contain spaces: fields are counted after its closing parenthesis.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return Process{}, fmt.Errorf("malformed %s/stat", dir)
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 20 {
		return Process{}, fmt.Errorf("malformed %s/stat", dir)
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64) // field 22: starttime
	if err != nil {
		return Process{}, fmt.Errorf("malformed %s/stat: %w", dir, err)
	}
	started := bootTime.Add(time.Duration(ticks) * time.Second / clockTicks)

	rss, err := readRSS(filepath.Join(dir, "status"))
	if err != nil {
		return Process{}, err
	}
	return Process{PID: pid, StartedAt: started, RSS: rss}, nil
}

func readRSS(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "VmRSS:"); ok {
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("malformed VmRSS in %s: %w", path, err)
			}
			return kb * 1024, nil
		}
	}
	return 0, s.Err() // no VmRSS: zombie or kernel thread
}

func readBootTime(procRoot string) (time.Time, error) {
	f, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("malformed btime in %s/stat: %w", procRoot, err)
			}
			return time.Unix(sec, 0), nil
		}
	}
	if err := s.Err(); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, fmt.Errorf("no btime in %s/stat", procRoot)
}
