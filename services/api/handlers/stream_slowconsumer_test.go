package handlers_test

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/handlers"
	"github.com/redis/go-redis/v9"
)

// parsePrometheusCounter extracts the value of a single-labelless counter
// line (e.g. "trident_sse_slow_consumer_disconnects_total 3") from a
// Prometheus text-exposition body. Returns 0 if the metric is absent.
func parsePrometheusCounter(body, metric string) int64 {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+" ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		return v
	}
	return 0
}

// slowConsumerRedis feeds Stream() an unbounded run of matching events so the
// write loop keeps trying to write for as long as the test needs it to.
type slowConsumerRedis struct {
	contractID string
}

func (r *slowConsumerRedis) XRevRangeN(ctx context.Context, key, start, stop string, count int64) *redis.XMessageSliceCmd {
	cmd := redis.NewXMessageSliceCmd(ctx)
	cmd.SetVal(nil) // empty tail -> Stream() starts from "0-0"
	return cmd
}

func (r *slowConsumerRedis) XRead(ctx context.Context, a *redis.XReadArgs) *redis.XStreamSliceCmd {
	cmd := redis.NewXStreamSliceCmd(ctx)
	// A ~4KB payload per message so a handful of writes overflow the OS
	// socket buffers of a client that never reads.
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'x'
	}
	cmd.SetVal([]redis.XStream{
		{
			Stream: "trident:events",
			Messages: []redis.XMessage{
				{
					ID: "1-1",
					Values: map[string]interface{}{
						"contract_id": r.contractID,
						"network":     "testnet",
						"topics":      `["transfer"]`,
						"padding":     string(big),
					},
				},
			},
		},
	})
	return cmd
}

// TestStream_SlowConsumerNeverReadingIsDisconnected simulates a real slow
// consumer end-to-end: a raw TCP client connects, completes the HTTP
// request, and then never reads the response body. The server must not
// block forever — the write deadline added for issue #224 must fire and the
// handler must return (verified by the server's ServeHTTP call completing
// and the disconnect metric advancing), instead of leaking the connection's
// goroutine indefinitely.
func TestStream_SlowConsumerNeverReadingIsDisconnected(t *testing.T) {
	before := testReadSSEDisconnectMetric()

	// A well-formed strkey contract id ('C' + 55 base32 chars) — Stream()
	// validates this shape before it will start streaming.
	const contractID = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", handlers.Stream(&slowConsumerRedis{contractID: contractID}))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	req := "GET /stream?contractId=" + contractID + " HTTP/1.1\r\nHost: " + addr + "\r\nConnection: keep-alive\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Read only the status line and headers, then stop reading entirely —
	// this is the "never reads" slow consumer. The server keeps writing SSE
	// frames into a socket buffer that is never drained by the client.
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if got := statusLine[:len("HTTP/1.1 200")]; got != "HTTP/1.1 200" {
		t.Fatalf("status line: got %q", statusLine)
	}

	// Give the write loop time to fill the kernel socket buffer and hit the
	// sseWriteDeadline (10s) at least once. Poll the metric instead of a
	// single fixed sleep so the test isn't flaky under slow CI runners.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if testReadSSEDisconnectMetric() > before {
			return // disconnected as expected — test passes
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Fatal("slow consumer was never disconnected within the expected window")
}

// testReadSSEDisconnectMetric scrapes the current SSE slow-consumer
// disconnect counter via the real MetricsHandler, so the assertion exercises
// the same counter the operator-facing /metrics endpoint exposes.
func testReadSSEDisconnectMetric() int64 {
	rec := httptest.NewRecorder()
	handlers.MetricsHandler(nil, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	return parsePrometheusCounter(body, "trident_sse_slow_consumer_disconnects_total")
}
