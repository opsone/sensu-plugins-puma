package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// State is the content of the Puma state file (state_path), written by the master at boot.
type State struct {
	PID              int
	RunningFrom      string
	ControlURL       string
	ControlAuthToken string
}

// ErrNoControlApp is returned by ReadState when the instance does not enable activate_control_app.
var ErrNoControlApp = errors.New("no control_url in state file (activate_control_app not set)")

// ReadState parses a Puma state file. It is a flat YAML document, read without a YAML library.
func ReadState(path string) (State, error) {
	f, err := os.Open(path)
	if err != nil {
		return State{}, err
	}
	defer f.Close()

	var st State
	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, ok := strings.Cut(s.Text(), ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "pid":
			st.PID, _ = strconv.Atoi(value)
		case "running_from":
			st.RunningFrom = value
		case "control_url":
			st.ControlURL = value
		case "control_auth_token":
			st.ControlAuthToken = value
		}
	}
	if err := s.Err(); err != nil {
		return State{}, err
	}
	if st.ControlURL == "" {
		return st, ErrNoControlApp
	}
	return st, nil
}

// ParseControl turns a --control value, a port on 127.0.0.1 or a tcp://host:port or unix:///abs/path
// URL, into a control URL.
func ParseControl(v string) (string, error) {
	if port, err := strconv.Atoi(v); err == nil {
		if port < 1 || port > 65535 {
			return "", fmt.Errorf("port %d out of range", port)
		}
		return "tcp://127.0.0.1:" + strconv.Itoa(port), nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "tcp":
		if u.Hostname() == "" || u.Port() == "" || (u.Path != "" && u.Path != "/") {
			return "", errors.New("want tcp://host:port")
		}
	case "unix":
		if u.Host != "" || !filepath.IsAbs(u.Path) {
			return "", errors.New("want unix:///absolute/path")
		}
	default:
		return "", errors.New("want a port, tcp://host:port or unix:///absolute/path")
	}
	return v, nil
}

// Stats is the cluster mode answer of the control app /stats endpoint.
type Stats struct {
	StartedAt     time.Time      `json:"started_at"`
	Workers       int            `json:"workers"`
	Phase         int            `json:"phase"`
	BootedWorkers int            `json:"booted_workers"`
	OldWorkers    int            `json:"old_workers"`
	WorkerStatus  []WorkerStatus `json:"worker_status"`
}

// WorkerStatus is the state of one cluster worker, as last reported to the master.
type WorkerStatus struct {
	StartedAt   time.Time    `json:"started_at"`
	PID         int          `json:"pid"`
	Index       int          `json:"index"`
	Phase       int          `json:"phase"`
	Booted      bool         `json:"booted"`
	LastCheckin time.Time    `json:"last_checkin"`
	LastStatus  ThreadStatus `json:"last_status"`
}

// ThreadStatus is the thread pool state of a worker.
type ThreadStatus struct {
	Backlog       int `json:"backlog"`
	Running       int `json:"running"`
	PoolCapacity  int `json:"pool_capacity"`
	MaxThreads    int `json:"max_threads"`
	RequestsCount int `json:"requests_count"`
}

// Busy is the number of threads currently processing a request.
func (t ThreadStatus) Busy() int {
	return max(t.MaxThreads-t.PoolCapacity, 0)
}

// FetchStats queries a control app, found in a state file or given with --control. Supports unix:// and tcp:// control URLs.
func FetchStats(ctx context.Context, st State) (*Stats, error) {
	u, err := url.Parse(st.ControlURL)
	if err != nil {
		return nil, fmt.Errorf("invalid control_url %q: %w", st.ControlURL, err)
	}

	client := &http.Client{}
	base := ""
	switch u.Scheme {
	case "unix":
		// unix:///abs/path.sock, or unix://relative/path.sock relative to the app directory.
		sock := u.Host + u.Path
		if !filepath.IsAbs(sock) && st.RunningFrom != "" {
			sock = filepath.Join(st.RunningFrom, sock)
		}
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}
		base = "http://puma"
	case "tcp":
		base = "http://" + u.Host
	default:
		return nil, fmt.Errorf("unsupported control_url scheme %q", u.Scheme)
	}

	path := "/stats"
	if st.ControlAuthToken != "" { // empty: control app started with no_token
		path += "?token=" + url.QueryEscape(st.ControlAuthToken)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("control app %s: %w", st.ControlURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("control app %s: %w", st.ControlURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("control app %s: HTTP %d", st.ControlURL, resp.StatusCode)
	}

	var stats Stats
	if err := json.Unmarshal(body, &stats); err != nil {
		return nil, fmt.Errorf("control app %s: decoding /stats: %w", st.ControlURL, err)
	}
	return &stats, nil
}
