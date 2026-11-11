package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/keys"
	"github.com/jclement/mapthing/internal/store"
)

// Review 3, finding 5: a request that is charged while the server drains, and then outlasts
// the grace period (a long download), still has its units written before the process ends.
func TestShutdownWritesUnitsChargedWhileDraining(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ks := keys.NewService(st)
	k := &store.APIKey{ID: 1, Limits: store.Limits{DailyQuota: 1000}}

	limiterDone := make(chan struct{})
	go func() {
		defer close(limiterDone)
		ks.Run(ctx, time.Hour) // only its final flush matters here
	}()

	started, charged := make(chan struct{}), make(chan struct{})
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// The limiter's own final flush (at the signal) has come and gone by the time this
		// request is charged.
		time.Sleep(150 * time.Millisecond)
		if d := ks.Charge(r.Context(), k, "pack", ""); !d.OK {
			t.Errorf("charge: %+v", d)
		}
		close(charged)
		<-r.Context().Done() // a download that goes on for longer than anyone waits
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/pack")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	begin := time.Now()
	shutdown(srv, ks, limiterDone, stop, 400*time.Millisecond)
	if took := time.Since(begin); took > 5*time.Second {
		t.Errorf("shutdown took %v with a request that never ends", took)
	}
	select {
	case <-charged:
	default:
		t.Fatal("test setup: the request was not charged during the drain")
	}
	// A new process on the same database knows about the 50 units.
	day := time.Now().UTC().Format("2006-01-02")
	if got, err := st.QuotaUsed(context.Background(), 1, day); err != nil || got != 50 {
		t.Fatalf("stored after shutdown: %d units (%v), want the 50 charged while draining", got, err)
	}
}
