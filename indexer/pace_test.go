package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
