package handlers_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/handlers"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/redis/go-redis/v9"
)

const fanoutContractID = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// fakeStreamRedis is an in-memory stand-in for the Redis Stream: XRead blocks
// until an entry newer than the cursor exists or the block time elapses, like
// the real command, and records how many reads are in flight at once.
type fakeStreamRedis struct {
	mu          sync.Mutex
	msgs        []redis.XMessage
	inflight    atomic.Int32
	maxInflight atomic.Int32
}

func seqOf(id string) int64 {
	n, _ := strconv.ParseInt(strings.SplitN(id, "-", 2)[0], 10, 64)
	return n
}

func (f *fakeStreamRedis) add(values map[string]any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("%d-0", len(f.msgs)+1)
	f.msgs = append(f.msgs, redis.XMessage{ID: id, Values: values})
	return id
}

func (f *fakeStreamRedis) after(cursor string) []redis.XMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []redis.XMessage
	for _, m := range f.msgs {
		if seqOf(m.ID) > seqOf(cursor) {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeStreamRedis) XRevRangeN(ctx context.Context, _, _, _ string, _ int64) *redis.XMessageSliceCmd {
	cmd := redis.NewXMessageSliceCmd(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.msgs); n > 0 {
		cmd.SetVal([]redis.XMessage{f.msgs[n-1]})
	}
	return cmd
}

func (f *fakeStreamRedis) XRead(ctx context.Context, a *redis.XReadArgs) *redis.XStreamSliceCmd {
	cmd := redis.NewXStreamSliceCmd(ctx)
	cur := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		old := f.maxInflight.Load()
		if cur <= old || f.maxInflight.CompareAndSwap(old, cur) {
			break
		}
	}
	deadline := time.Now().Add(a.Block)
	for {
		if msgs := f.after(a.Streams[1]); len(msgs) > 0 {
			cmd.SetVal([]redis.XStream{{Stream: a.Streams[0], Messages: msgs}})
			return cmd
		}
		if a.Block < 0 || time.Now().After(deadline) || ctx.Err() != nil {
			cmd.SetErr(redis.Nil)
			return cmd
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func openSSE(t *testing.T, h http.Handler, query string) (*bufio.Scanner, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/s?"+query, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		srv.Close()
		t.Fatalf("connect: %v", err)
	}
	return bufio.NewScanner(resp.Body), func() { cancel(); _ = resp.Body.Close(); srv.Close() }
}

func TestStreamSendsKeepaliveCommentWhenQuiet(t *testing.T) {
	defer handlers.SetSSEHeartbeatInterval(40 * time.Millisecond)()
	mux := http.NewServeMux()
	mux.HandleFunc("/s", handlers.Stream(&fakeStreamRedis{}))
	sc, stop := openSSE(t, mux, "contractId="+fanoutContractID)
	defer stop()

	got := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), ":") {
				got <- sc.Text()
				return
			}
		}
	}()
	select {
	case line := <-got:
		if !strings.HasPrefix(line, ": ping") {
			t.Fatalf("keepalive line = %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no keepalive comment on a quiet stream")
	}
}

func TestStreamSharesOneRedisReaderAcrossClients(t *testing.T) {
	fake := &fakeStreamRedis{}
	mux := http.NewServeMux()
	mux.HandleFunc("/s", handlers.Stream(fake))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const clients = 40
	scanners := make([]*bufio.Scanner, clients)
	for i := range scanners {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/s?contractId="+fanoutContractID, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		defer func() { _ = resp.Body.Close() }()
		scanners[i] = bufio.NewScanner(resp.Body)
	}

	fake.add(map[string]any{"contract_id": fanoutContractID, "network": "testnet", "topics": `["t"]`, "id": "evt-1"})
	var wg sync.WaitGroup
	for i, sc := range scanners {
		wg.Add(1)
		go func(i int, sc *bufio.Scanner) {
			defer wg.Done()
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "data: ") {
					return
				}
			}
			t.Errorf("client %d got no event", i)
		}(i, sc)
	}
	wg.Wait()

	if max := fake.maxInflight.Load(); max != 1 {
		t.Fatalf("max concurrent Redis reads = %d for %d clients, want 1", max, clients)
	}
}

func TestStreamFiltersNetworkAndExposesEventID(t *testing.T) {
	fake := &fakeStreamRedis{}
	mux := http.NewServeMux()
	mux.Handle("/s", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Stream(fake)(w, r.WithContext(middleware.WithNetwork(r.Context(), "mainnet")))
	}))
	sc, stop := openSSE(t, mux, "contractId="+fanoutContractID)
	defer stop()
	time.Sleep(100 * time.Millisecond)

	base := func(network, id string) map[string]any {
		return map[string]any{"contract_id": fanoutContractID, "network": network, "topics": `["t"]`, "id": id, "event_id": id}
	}
	fake.add(base("testnet", "testnet-evt"))
	fake.add(base("mainnet", "mainnet-evt"))

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if strings.Contains(line, "testnet-evt") {
			t.Fatalf("mainnet subscriber received a testnet entry: %s", line)
		}
		if !strings.Contains(line, `"id":"mainnet-evt"`) {
			t.Fatalf("payload lacks the event id: %s", line)
		}
		return
	}
	t.Fatal("stream ended without the mainnet event")
}
