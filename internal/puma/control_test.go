package puma

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const statsJSON = `{"started_at":"2026-11-08T10:00:00Z","workers":2,"phase":1,"booted_workers":2,"old_workers":1,
"worker_status":[
 {"started_at":"2026-11-08T10:00:01Z","pid":101,"index":0,"phase":1,"booted":true,"last_checkin":"2026-11-08T12:00:00Z",
  "last_status":{"backlog":0,"running":5,"pool_capacity":2,"max_threads":5,"requests_count":42,"busy_threads":3}},
 {"started_at":"2026-11-08T09:00:01Z","pid":102,"index":1,"phase":0,"booted":true,"last_checkin":"2026-11-08T12:00:02Z",
  "last_status":{"backlog":3,"running":5,"pool_capacity":0,"max_threads":5,"requests_count":7}}]}`

func TestReadState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "puma.state")
	content := "---\npid: 12345\nrunning_from: /var/www/myapp_prod/releases/20261108\ncontrol_url: unix:///var/www/myapp_prod/shared/tmp/sockets/pumactl.sock\ncontrol_auth_token: 'abc123'\nrunning: true\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(path)
	if err != nil {
		t.Fatal(err)
	}
	want := State{
		PID:              12345,
		RunningFrom:      "/var/www/myapp_prod/releases/20261108",
		ControlURL:       "unix:///var/www/myapp_prod/shared/tmp/sockets/pumactl.sock",
		ControlAuthToken: "abc123",
	}
	if got != want {
		t.Errorf("ReadState() = %+v, want %+v", got, want)
	}
}

func TestReadStateWithoutControlApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "puma.state")
	if err := os.WriteFile(path, []byte("---\npid: 12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(path); !errors.Is(err, ErrNoControlApp) {
		t.Errorf("ReadState() error = %v, want ErrNoControlApp", err)
	}
}

func statsHandler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats" || r.URL.Query().Get("token") != "s3cr&t" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Write([]byte(statsJSON))
	})
}

func TestFetchStatsUnix(t *testing.T) {
	// Short directory: unix socket paths are limited to ~104 bytes on macOS.
	dir, err := os.MkdirTemp("", "puma")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Mkdir(filepath.Join(dir, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "tmp", "ctl.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(statsHandler(t))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Absolute and relative (to running_from) socket paths.
	for _, u := range []string{"unix://" + filepath.Join(dir, "tmp", "ctl.sock"), "unix://tmp/ctl.sock"} {
		stats, err := FetchStats(ctx, State{RunningFrom: dir, ControlURL: u, ControlAuthToken: "s3cr&t"})
		if err != nil {
			t.Fatalf("FetchStats(%s): %v", u, err)
		}
		checkStats(t, stats)
	}
}

func TestFetchStatsTCP(t *testing.T) {
	srv := httptest.NewServer(statsHandler(t))
	t.Cleanup(srv.Close)
	ctx := context.Background()

	u := strings.Replace(srv.URL, "http://", "tcp://", 1)
	stats, err := FetchStats(ctx, State{ControlURL: u, ControlAuthToken: "s3cr&t"})
	if err != nil {
		t.Fatal(err)
	}
	checkStats(t, stats)

	if _, err := FetchStats(ctx, State{ControlURL: u, ControlAuthToken: "wrong"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("FetchStats() with a wrong token: error = %v, want HTTP 403", err)
	}
	if _, err := FetchStats(ctx, State{ControlURL: "ssl://127.0.0.1:9293"}); err == nil {
		t.Error("FetchStats() with ssl://: want an unsupported scheme error")
	}
}

func checkStats(t *testing.T, s *Stats) {
	t.Helper()
	if s.Workers != 2 || s.BootedWorkers != 2 || s.Phase != 1 || s.OldWorkers != 1 || len(s.WorkerStatus) != 2 {
		t.Fatalf("unexpected stats: %+v", s)
	}
	w := s.WorkerStatus[1]
	if w.PID != 102 || w.Index != 1 || w.Phase != 0 || !w.Booted || w.LastStatus.Backlog != 3 || w.LastStatus.Busy() != 5 {
		t.Errorf("unexpected worker status: %+v", w)
	}
	if want := time.Date(2026, 11, 8, 12, 0, 2, 0, time.UTC); !w.LastCheckin.Equal(want) {
		t.Errorf("last_checkin = %v, want %v", w.LastCheckin, want)
	}
	if busy := s.WorkerStatus[0].LastStatus.Busy(); busy != 3 {
		t.Errorf("worker 0 busy = %d, want 3", busy)
	}
}
