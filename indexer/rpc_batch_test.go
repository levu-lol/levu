package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stub answers JSON-RPC batches and counts the HTTP requests it took.
func stub(t *testing.T, maxBatch int) (*httptest.Server, *int64) {
	t.Helper()
	var requests int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, "bad", 400)
			return
		}
		var batch []rpcReq
		if err := json.Unmarshal(raw, &batch); err != nil {
			var one rpcReq
			if err := json.Unmarshal(raw, &one); err != nil {
				http.Error(w, "bad", 400)
				return
			}
			batch = []rpcReq{one}
		}
		if maxBatch > 0 && len(batch) > maxBatch {
			// What the real endpoint does with an over-expensive batch.
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		out := make([]rpcResp, 0, len(batch))
		for _, q := range batch {
			// A 32-byte word holding the id, so each answer is distinguishable.
			word := fmt.Sprintf("0x%064x", q.ID+1)
			out = append(out, rpcResp{ID: q.ID, Result: json.RawMessage(`"` + word + `"`)})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// The point of the whole exercise: concurrent calls become one request.
func TestConcurrentCallsShareOneRequest(t *testing.T) {
	srv, requests := stub(t, 0)
	// MaxBatch above n, so the assertion is about coalescing and not about the
	// cap. That the cap also works is TestABatchIsCapped...'s job.
	c := &RPCChain{RPC: srv.URL, BatchWindow: 25 * time.Millisecond, MaxBatch: 64}

	const n = 30
	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := c.Call(context.Background(),
				"0x0000000000000000000000000000000000000001", "liquidity()(uint128)")
			errs[i] = err
			if len(out) > 0 {
				got[i] = out[0]
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got[i] == "" {
			t.Fatalf("call %d got nothing", i)
		}
	}
	if r := atomic.LoadInt64(requests); r != 1 {
		t.Fatalf("%d calls took %d HTTP requests, want 1", n, r)
	}
	// Every caller must get *its own* answer, not whichever came back first.
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	if len(seen) != n {
		t.Fatalf("%d distinct answers for %d callers: results were crossed", len(seen), n)
	}
}

// A batch never exceeds MaxBatch, because the endpoint refuses one that does.
func TestABatchIsCappedSoTheEndpointAccceptsIt(t *testing.T) {
	const cap = 8
	srv, requests := stub(t, cap)
	c := &RPCChain{RPC: srv.URL, BatchWindow: 25 * time.Millisecond, MaxBatch: cap}

	var wg sync.WaitGroup
	errs := make([]error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Call(context.Background(),
				"0x0000000000000000000000000000000000000001", "liquidity()(uint128)")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d refused, so a batch went out over the cap: %v", i, err)
		}
	}
	if r := atomic.LoadInt64(requests); r < 5 {
		t.Fatalf("40 calls in %d requests: the cap was not applied", r)
	}
}

// One caller's failure is its own. A batch that comes back with an error for
// one id must not fail the others.
func TestOneBadCallDoesNotPoisonItsBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []rpcReq
		_ = json.NewDecoder(r.Body).Decode(&batch)
		out := make([]rpcResp, 0, len(batch))
		for _, q := range batch {
			if q.ID%2 == 0 {
				out = append(out, rpcResp{ID: q.ID, Error: &struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				}{Code: -32000, Message: "execution reverted"}})
				continue
			}
			out = append(out, rpcResp{ID: q.ID,
				Result: json.RawMessage(`"0x` + strings.Repeat("0", 63) + `7"`)})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	c := &RPCChain{RPC: srv.URL, BatchWindow: 25 * time.Millisecond}
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Call(context.Background(),
				"0x0000000000000000000000000000000000000001", "liquidity()(uint128)")
		}(i)
	}
	wg.Wait()

	var failed, ok int
	for _, err := range errs {
		if err != nil {
			failed++
		} else {
			ok++
		}
	}
	if failed == 0 || ok == 0 {
		t.Fatalf("expected a mix: %d failed, %d ok", failed, ok)
	}
}

// A caller that gives up must not wedge the batch behind it.
func TestACancelledCallerDoesNotHoldUpTheRest(t *testing.T) {
	srv, _ := stub(t, 0)
	c := &RPCChain{RPC: srv.URL, BatchWindow: 20 * time.Millisecond}

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Call(dead, "0x0000000000000000000000000000000000000001",
		"liquidity()(uint128)"); err == nil {
		t.Fatal("a cancelled call returned success")
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(),
			"0x0000000000000000000000000000000000000001", "liquidity()(uint128)")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the next call failed after a cancelled one: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the queue wedged behind a cancelled caller")
	}
}
