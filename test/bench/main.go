// Command bench measures the latency a gateway adds and the throughput it
// sustains, against a fake upstream that answers at once.
//
// It has two modes. With -upstream it serves the fake upstream: an
// OpenAI-compatible /v1/chat/completions that answers a fixed completion
// after -delay. Otherwise it sends -n chat completions with -c concurrent
// clients to each -target and prints a table of p50, p90 and p99 latency
// and requests per second. The first target is the baseline (usually the
// fake upstream itself), and every other row shows the latency it adds
// over that baseline at each percentile.
//
//	bench -upstream 127.0.0.1:18291
//	bench -target direct=http://127.0.0.1:18291/v1 -target gateway=http://127.0.0.1:18290/v1 -key k
//
// scripts/bench.sh runs the whole comparison; docs/benchmarks.md has the
// method and results.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type targets []target

type target struct {
	name, url, key string
	headers        [][2]string
}

func (t *targets) String() string { return fmt.Sprint(*t) }

// Set parses name=url, with optional ,key=secret for the target's own key
// and ,header=Name:Value for extra request headers.
func (t *targets) Set(v string) error {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || rest == "" {
		return errors.New("want name=url[,key=secret][,header=Name:Value]")
	}
	parts := strings.Split(rest, ",")
	tg := target{name: name, url: strings.TrimRight(parts[0], "/")}
	for _, p := range parts[1:] {
		k, val, _ := strings.Cut(p, "=")
		switch k {
		case "key":
			tg.key = val
		case "header":
			hn, hv, ok := strings.Cut(val, ":")
			if !ok {
				return fmt.Errorf("header %q: want Name:Value", val)
			}
			tg.headers = append(tg.headers, [2]string{hn, hv})
		default:
			return fmt.Errorf("unknown target option %q", k)
		}
	}
	*t = append(*t, tg)
	return nil
}

func main() {
	var ts targets
	upstream := flag.String("upstream", "", "serve the fake upstream on this address instead of benchmarking")
	delay := flag.Duration("delay", 0, "fake upstream: time to wait before answering")
	n := flag.Int("n", 20000, "requests per target")
	c := flag.Int("c", 32, "concurrent clients")
	warmup := flag.Int("warmup", 1000, "requests per target before measuring")
	model := flag.String("model", "bench", "model to request")
	key := flag.String("key", "bench-key", "bearer key for targets without their own")
	stream := flag.Bool("stream", false, "request streamed completions")
	format := flag.String("format", "text", "output: text or markdown")
	maxAdded := flag.Duration("max-added-p50", 0, "fail when a target adds more than this at p50 over the baseline (0: no gate)")
	flag.Var(&ts, "target", "name=base_url[,key=secret]; repeat, the first is the baseline")
	flag.Parse()

	if *upstream != "" {
		log.Fatal(serveUpstream(*upstream, *delay))
	}
	if len(ts) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns:        *c * 2,
		MaxIdleConnsPerHost: *c * 2,
		IdleConnTimeout:     time.Minute,
	}}
	var results []result
	for _, t := range ts {
		if t.key == "" {
			t.key = *key
		}
		run(context.Background(), client, t, *model, *stream, *warmup, *c)
		r := run(context.Background(), client, t, *model, *stream, *n, *c)
		results = append(results, r)
		fmt.Fprintf(os.Stderr, "%s: %d requests, %d errors, %.0f req/s\n", t.name, r.count, r.errors, r.rps)
	}
	report(os.Stdout, results, *format, *n, *c, *stream)
	for _, r := range results {
		if r.errors > 0 {
			os.Exit(1)
		}
		if *maxAdded > 0 && r.p50-results[0].p50 > *maxAdded {
			fmt.Fprintf(os.Stderr, "%s adds %v at p50, over the %v gate\n", r.name, r.p50-results[0].p50, *maxAdded)
			os.Exit(1)
		}
	}
}

type result struct {
	name     string
	count    int
	errors   int64
	rps      float64
	p50, p90 time.Duration
	p99      time.Duration
}

// run sends n requests from c clients and collects their latencies.
func run(ctx context.Context, client *http.Client, t target, model string, stream bool, n, c int) result {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "Say hello in one word."}},
		"stream":   stream,
	})
	lat := make([]time.Duration, n)
	var next, errs atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for range c {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				t0 := time.Now()
				if err := call(ctx, client, t, body); err != nil {
					if errs.Add(1) <= 3 {
						fmt.Fprintf(os.Stderr, "%s: %v\n", t.name, err)
					}
				}
				lat[i] = time.Since(t0)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	slices.Sort(lat)
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[min(len(lat)-1, int(float64(len(lat))*p))]
	}
	return result{
		name: t.name, count: n, errors: errs.Load(),
		rps: float64(n) / elapsed.Seconds(),
		p50: pct(0.50), p90: pct(0.90), p99: pct(0.99),
	}
}

func call(ctx context.Context, client *http.Client, t target, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.key)
	for _, h := range t.headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %.200s", resp.StatusCode, data)
	}
	return nil
}

func report(w io.Writer, rs []result, format string, n, c int, stream bool) {
	base := rs[0]
	ms := func(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d.Microseconds())/1000) }
	added := func(d, b time.Duration) string {
		if d < b {
			return "+0.00"
		}
		return "+" + ms(d-b)
	}
	if format == "markdown" {
		fmt.Fprintf(w, "%d requests per target, %d concurrent clients, stream=%v.\n\n", n, c, stream)
		fmt.Fprintln(w, "| Target | p50 ms | p90 ms | p99 ms | Added p50 ms | Added p99 ms | Requests/s |")
		fmt.Fprintln(w, "| --- | ---: | ---: | ---: | ---: | ---: | ---: |")
		for i, r := range rs {
			a50, a99 := added(r.p50, base.p50), added(r.p99, base.p99)
			if i == 0 {
				a50, a99 = "baseline", "baseline"
			}
			fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s | %.0f |\n", r.name, ms(r.p50), ms(r.p90), ms(r.p99), a50, a99, r.rps)
		}
		return
	}
	fmt.Fprintf(w, "%-16s %9s %9s %9s %10s %10s %10s\n", "target", "p50 ms", "p90 ms", "p99 ms", "+p50 ms", "+p99 ms", "req/s")
	for _, r := range rs {
		fmt.Fprintf(w, "%-16s %9s %9s %9s %10s %10s %10.0f\n", r.name, ms(r.p50), ms(r.p90), ms(r.p99),
			added(r.p50, base.p50), added(r.p99, base.p99), r.rps)
	}
}

// serveUpstream answers chat completions like OpenAI, after delay.
func serveUpstream(addr string, delay time.Duration) error {
	completion := []byte(`{"id":"chatcmpl-bench","object":"chat.completion","created":1,"model":"bench-upstream",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"Hello."},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}}`)
	chunks := []string{
		`{"id":"chatcmpl-bench","object":"chat.completion.chunk","created":1,"model":"bench-upstream","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello."},"finish_reason":null}]}`,
		`{"id":"chatcmpl-bench","object":"chat.completion.chunk","created":1,"model":"bench-upstream","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-bench","object":"chat.completion.chunk","created":1,"model":"bench-upstream","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"bench-upstream","object":"model"}]}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(completion)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
}
