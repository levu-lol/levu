package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func okServer(hits *int32, refuseFirst int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(hits, 1)
		if n <= refuseFirst {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		var reqs []rpcReq
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		out := make([]rpcResp, len(reqs))
		for i, q := range reqs {
			out[i] = rpcResp{ID: q.ID, Result: json.RawMessage(`"0x0"`)}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// Requests go out at the configured pace, however fast callers ask.
func TestRequestsArePaced(t *testing.T) {
	var hits int32
	srv := okServer(&hits, 0)
	defer srv.Close()
	c := &RPCChain{RPC: srv.URL, Rate: 10, BatchWindow: -1} // no batching: one request per call
	start := time.Now()
	for i := 0; i < 6; i++ {
		var out any
		if err := c.do(context.Background(), "eth_blockNumber", nil, &out); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el < 450*time.Millisecond {
		t.Fatalf("6 requests at 10/s took %v; they were not paced", el)
	}
}

// A refusal holds the client rather than multiplying its requests.
//
// The client used to answer a 429 by splitting the batch and sending both
// halves at once, recursively -- one refused request becoming two, four,
// eight against the limiter that had just refused it. On 2026-09-05 that was
// 200-570 refusals every ten minutes and half the markets on a stale price.
func TestARefusalHoldsInsteadOfFanningOut(t *testing.T) {
	var hits int32
	srv := okServer(&hits, 1) // the first request is refused with Retry-After: 1
	defer srv.Close()
	c := &RPCChain{RPC: srv.URL, Rate: 100, BatchWindow: -1}
	start := time.Now()
	var out any
	if err := c.do(context.Background(), "eth_blockNumber", nil, &out); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 900*time.Millisecond {
		t.Fatalf("retried %v after a Retry-After of 1s; the refusal was not honoured", el)
	}
	if h := atomic.LoadInt32(&hits); h != 2 {
		t.Fatalf("%d requests to get one answer after one refusal; want exactly 2", h)
	}
	if c.Hold() > 0 {
		t.Fatal("the hold outlived the retry that succeeded")
	}
}

// A client asked for more than it will send sheds the excess at once.
//
// Queueing it is the failure this guards against: every request booked later
// than the last, each one dead at its deadline before it was sent, and the
// whole feed down with not one refusal in the log.
func TestOverloadFailsFastInsteadOfQueueingToDeath(t *testing.T) {
	var hits int32
	srv := okServer(&hits, 0)
	defer srv.Close()
	c := &RPCChain{RPC: srv.URL, Rate: 2, BatchWindow: -1} // 0.5s per request
	// Forty callers at once: twenty seconds of work at this pace, all demanded
	// in the same instant. That is what fifty-seven market goroutines look
	// like to the client, and it is the shape the first version queued to
	// death.
	var dropped, sent int32
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var out any
			switch err := c.do(ctx, "eth_blockNumber", nil, &out); {
			case errors.Is(err, ErrOverPace):
				atomic.AddInt32(&dropped, 1)
			case err == nil:
				atomic.AddInt32(&sent, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	el := time.Since(start)
	if dropped == 0 {
		t.Fatalf("40 concurrent requests at 2/s all queued (%v): overload was not shed", el)
	}
	if sent < 3 {
		t.Fatalf("only %d sent: the backlog should still carry ~2s of work", sent)
	}
	if el > 4*time.Second {
		t.Fatalf("took %v: excess should be refused at once, not after waiting", el)
	}
	t.Logf("sent %d, dropped %d in %v", sent, dropped, el.Round(time.Millisecond))
}

// Calls arriving together share a request.
func TestConcurrentCallsCoalesceIntoOneRequest(t *testing.T) {
	var hits int32
	srv := okServer(&hits, 0)
	defer srv.Close()
	c := &RPCChain{RPC: srv.URL, Rate: 100, BatchWindow: 100 * time.Millisecond, MaxBatch: 50}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out any
			_ = c.do(context.Background(), "eth_blockNumber", nil, &out)
		}()
	}
	wg.Wait()
	if h := atomic.LoadInt32(&hits); h > 2 {
		t.Fatalf("30 concurrent calls took %d requests; they should share one or two", h)
	}
}
