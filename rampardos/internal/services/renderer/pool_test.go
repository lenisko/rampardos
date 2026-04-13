package renderer

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

func TestPoolServialisesRequests(t *testing.T) {
	pool, err := newStylePool(stylePoolConfig{
		styleID:          "fake",
		poolSize:         2,
		workerLifetime:   100,
		handshakeTimeout: 2 * time.Second,
		spawn: func() (*worker, error) {
			return spawnWorker(workerArgs{
				binary:           "bash",
				script:           "testdata/fake-worker-ok.sh",
				styleID:          "fake",
				handshakeTimeout: 2 * time.Second,
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.close()

	// Fire many concurrent requests; all should succeed and all
	// return the canned payload.
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			payload, err := pool.dispatch(ctx, []byte(`{"zoom":14}`))
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(payload, []byte("fake")) {
				errs <- errPayload(payload)
				return
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("dispatch failure: %v", err)
	}
}

func TestPoolRecyclesAfterLifetime(t *testing.T) {
	pool, err := newStylePool(stylePoolConfig{
		styleID:          "fake",
		poolSize:         1,
		workerLifetime:   3, // recycle every 3 renders
		handshakeTimeout: 2 * time.Second,
		spawn: func() (*worker, error) {
			return spawnWorker(workerArgs{
				binary:           "bash",
				script:           "testdata/fake-worker-ok.sh",
				styleID:          "fake",
				handshakeTimeout: 2 * time.Second,
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.close()

	pids := map[int]bool{}
	for range 10 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := pool.dispatch(ctx, []byte(`{}`))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		pids[pool.lastWorkerPIDForTest()] = true
	}

	// With lifetime=3 and 10 renders, we expect at least ceil(10/3) = 4 distinct workers.
	if len(pids) < 4 {
		t.Errorf("expected at least 4 distinct worker PIDs across 10 renders, got %d", len(pids))
	}
}

type errPayload []byte

func (e errPayload) Error() string { return "unexpected payload: " + string(e) }
