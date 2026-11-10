package supervisor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The test binary doubles as a fake service: with FAKE_SERVICE_PORT set it serves
// /status (after FAKE_DELAY_MS; 500 forever with FAKE_FAIL=1) and /gen (FAKE_GEN).
func TestMain(m *testing.M) {
	if port := os.Getenv("FAKE_SERVICE_PORT"); port != "" {
		fakeService(port)
		return
	}
	os.Exit(m.Run())
}

func fakeService(port string) {
	delay, _ := strconv.Atoi(os.Getenv("FAKE_DELAY_MS"))
	start := time.Now()
	ready := func() bool {
		return os.Getenv("FAKE_FAIL") != "1" && time.Since(start) >= time.Duration(delay)*time.Millisecond
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "loading", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"version":"fake"}`)
	})
	mux.HandleFunc("/gen", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, os.Getenv("FAKE_GEN"))
	})
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux, ReadHeaderTimeout: time.Second}
	_ = srv.ListenAndServe()
	os.Exit(1)
}

func freePorts(t *testing.T) [2]int {
	t.Helper()
	var out [2]int
	for i := range out {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		out[i] = l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
	}
	return out
}

type fakeCfg struct {
	delayMS int
	fail    bool
}

func newBG(t *testing.T, sup *Supervisor, cfgs map[string]fakeCfg, switched *atomic.Value) *BlueGreen {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &BlueGreen{
		Name: "fake", Sup: sup, Ports: freePorts(t),
		Child: func(gen string, port int) (Child, error) {
			c := cfgs[gen]
			env := []string{"FAKE_SERVICE_PORT=" + strconv.Itoa(port), "FAKE_GEN=" + gen, "FAKE_DELAY_MS=" + strconv.Itoa(c.delayMS)}
			if c.fail {
				env = append(env, "FAKE_FAIL=1")
			}
			return Child{Command: []string{exe}, Env: env, HealthURL: fmt.Sprintf("http://127.0.0.1:%d/status", port)}, nil
		},
		Switch:       func(base string) { switched.Store(base) },
		ReadyTimeout: 5 * time.Second,
		PollEvery:    50 * time.Millisecond,
	}
}

func get(base, path string) (string, error) {
	resp, err := http.Get(base + path)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s", resp.Status)
	}
	return string(b), nil
}

func TestBlueGreenSwapWithoutDowntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New("")
	sup.Start(ctx)
	var base atomic.Value
	bg := newBG(t, sup, map[string]fakeCfg{"g1": {}, "g2": {delayMS: 700}}, &base)

	if err := bg.Activate(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	if g, err := get(base.Load().(string), "/gen"); err != nil || g != "g1" {
		t.Fatalf("g1: %q %v", g, err)
	}
	first := base.Load().(string)

	// hammer whatever the clients currently point at while g2 starts and takes over
	var errs, reqs atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			reqs.Add(1)
			if _, err := get(base.Load().(string), "/status"); err != nil {
				errs.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	sawStandby := make(chan bool, 1)
	go func() {
		for range 100 {
			for _, st := range sup.Statuses() {
				if st.Standby {
					sawStandby <- true
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		sawStandby <- false
	}()
	if err := bg.Activate(ctx, "g2"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	if errs.Load() != 0 {
		t.Fatalf("%d of %d requests failed during the swap", errs.Load(), reqs.Load())
	}
	if !<-sawStandby {
		t.Fatal("the new instance never showed as standby while warming up")
	}
	now := base.Load().(string)
	if now == first {
		t.Fatal("clients were not switched")
	}
	if g, err := get(now, "/gen"); err != nil || g != "g2" {
		t.Fatalf("after swap: %q %v", g, err)
	}
	if _, err := get(first, "/status"); err == nil {
		t.Fatal("old instance still running")
	}
	sts := sup.Statuses()
	if len(sts) != 1 || sts[0].Standby || !sts[0].Healthy {
		t.Fatalf("statuses after swap: %+v", sts)
	}
	if name, gen := bg.Active(); gen != "g2" || name != sts[0].Name {
		t.Fatalf("active %s %s", name, gen)
	}
}

func TestBlueGreenKeepsOldWhenNewFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New("")
	sup.Start(ctx)
	var base atomic.Value
	bg := newBG(t, sup, map[string]fakeCfg{"g1": {}, "bad": {fail: true}}, &base)
	bg.ReadyTimeout = 1500 * time.Millisecond
	if err := bg.Activate(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	first := base.Load().(string)
	if err := bg.Activate(ctx, "bad"); err == nil {
		t.Fatal("expected the failing instance to be refused")
	}
	if base.Load().(string) != first {
		t.Fatal("clients switched to a failing instance")
	}
	if g, err := get(first, "/gen"); err != nil || g != "g1" {
		t.Fatalf("old instance: %q %v", g, err)
	}
	if sts := sup.Statuses(); len(sts) != 1 {
		t.Fatalf("failed standby not removed: %+v", sts)
	}
	if _, gen := bg.Active(); gen != "g1" {
		t.Fatalf("active gen %s", gen)
	}
}

func TestBlueGreenUnavailableBinary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New("")
	sup.Start(ctx)
	bg := &BlueGreen{Name: "ghost", Sup: sup, Ports: freePorts(t),
		Child: func(string, int) (Child, error) {
			return Child{Command: []string{"definitely-not-a-real-binary-xyz"}}, nil
		}}
	err := bg.Activate(ctx, "g1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if sts := sup.Statuses(); len(sts) != 1 || sts[0].State != StateUnavailable {
		t.Fatalf("unavailable child should stay visible: %+v", sts)
	}
}

// Prepare brings the new instance up without touching the service; Abort leaves everything
// as it was, and a later activation still works.
func TestBlueGreenPrepareAbortAndCommit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New("")
	sup.Start(ctx)
	var base atomic.Value
	bg := newBG(t, sup, map[string]fakeCfg{"g1": {}, "g2": {}, "g3": {}}, &base)
	if err := bg.Activate(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	first := base.Load().(string)

	st, err := bg.Prepare(ctx, "g2")
	if err != nil {
		t.Fatal(err)
	}
	// Ready, but not serving: clients and the old instance are untouched.
	if base.Load().(string) != first {
		t.Fatal("Prepare switched clients")
	}
	if g, err := get(first, "/gen"); err != nil || g != "g1" {
		t.Fatalf("old instance during a prepared swap: %q %v", g, err)
	}
	if _, gen := bg.Active(); gen != "g1" {
		t.Fatalf("active generation %q before commit", gen)
	}
	standby := ""
	for _, s := range sup.Statuses() {
		if s.Standby {
			standby = s.Name
		}
	}
	if standby == "" {
		t.Fatalf("no standby instance: %+v", sup.Statuses())
	}
	st.Abort()
	st.Abort()  // harmless
	st.Commit() // too late: must not switch
	if base.Load().(string) != first {
		t.Fatal("clients switched after Abort")
	}
	if g, err := get(first, "/gen"); err != nil || g != "g1" {
		t.Fatalf("old instance after Abort: %q %v", g, err)
	}
	if sts := sup.Statuses(); len(sts) != 1 || sts[0].Standby {
		t.Fatalf("statuses after Abort: %+v", sts)
	}
	if _, gen := bg.Active(); gen != "g1" {
		t.Fatalf("active generation %q after Abort", gen)
	}

	// The same generation can be prepared again and committed.
	st, err = bg.Prepare(ctx, "g2")
	if err != nil {
		t.Fatal(err)
	}
	st.Commit()
	if g, err := get(base.Load().(string), "/gen"); err != nil || g != "g2" {
		t.Fatalf("after commit: %q %v", g, err)
	}
	// Preparing what is already active is a no-op that still has to be released.
	st, err = bg.Prepare(ctx, "g2")
	if err != nil {
		t.Fatal(err)
	}
	st.Commit()
	if err := bg.Activate(ctx, "g3"); err != nil {
		t.Fatal(err)
	}
	if g, err := get(base.Load().(string), "/gen"); err != nil || g != "g3" {
		t.Fatalf("after g3: %q %v", g, err)
	}
}
