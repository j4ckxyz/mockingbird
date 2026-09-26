// Package loadtest simulates thousands of vintage clients polling the
// bridge, against an in-memory fake PDS with configurable latency, and
// reports client latency, translation overhead (time not spent waiting on
// upstream), errors and memory.
//
// Run it on the target machine (e.g. the Pi 5) with:
//
//	mockingbird loadtest -users 3000 -rps 100 -duration 2m
package loadtest

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackgilbert/mockingbird/internal/app"
	"github.com/jackgilbert/mockingbird/internal/config"
	"github.com/jackgilbert/mockingbird/internal/fakepds"
)

// Result summarises a run.
type Result struct {
	Requests      int64
	Errors        int64
	Duration      time.Duration
	AchievedRPS   float64
	ClientP50     time.Duration
	ClientP95     time.Duration
	ClientP99     time.Duration
	OverheadP50   time.Duration
	OverheadP95   time.Duration
	OverheadP99   time.Duration
	UpstreamCalls int
	Logins        int
	HeapInuseMB   float64
	MaxRSSMB      float64
	ByRoute       map[string]int64
	WireBytes     int64
	BridgeHeapMB  float64
}

// Options configure a run.
type Options struct {
	Users           int
	RPS             float64
	Duration        time.Duration
	UpstreamLatency time.Duration
	PostsPerUser    int
	Follows         int
	Workers         int
	HeapProfile     string
	Out             io.Writer
}

// Main is the CLI entry point (mockingbird loadtest ...).
func Main(args []string) int {
	fs := flag.NewFlagSet("loadtest", flag.ExitOnError)
	o := Options{Out: os.Stdout}
	fs.IntVar(&o.Users, "users", 3000, "simulated accounts")
	fs.Float64Var(&o.RPS, "rps", 100, "target request rate (3000 users polling two endpoints every 2.5 min is ~40)")
	fs.DurationVar(&o.Duration, "duration", time.Minute, "measurement duration")
	fs.DurationVar(&o.UpstreamLatency, "upstream-latency", 60*time.Millisecond, "simulated PDS/AppView latency per call")
	fs.IntVar(&o.PostsPerUser, "posts", 20, "posts per simulated account")
	fs.IntVar(&o.Follows, "follows", 50, "accounts each simulated user follows")
	fs.IntVar(&o.Workers, "workers", 256, "maximum concurrent in-flight requests")
	fs.StringVar(&o.HeapProfile, "heapprofile", "", "write a heap profile here at the end")
	fs.Parse(args)
	res, err := Run(context.Background(), o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		return 1
	}
	Print(o.Out, o, res)
	if res.OverheadP95 > 50*time.Millisecond {
		return 2
	}
	return 0
}

// Run executes a load test.
func Run(ctx context.Context, o Options) (*Result, error) {
	const password = "load-test-pass-word"
	pds := fakepds.New()
	pds.Latency = o.UpstreamLatency
	pdsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	pdsSrv := &http.Server{Handler: pds}
	go pdsSrv.Serve(pdsLn)
	defer pdsSrv.Close()
	pds.URL = "http://" + pdsLn.Addr().String()
	fmt.Fprintf(o.Out, "seeding %d accounts x %d posts...\n", o.Users, o.PostsPerUser)
	pds.Seed(o.Users, o.Follows, o.PostsPerUser, password)
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	dir, err := os.MkdirTemp("", "mockingbird-loadtest-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	env := map[string]string{
		"MB_SECRET_KEY": strings.Repeat("ab", 32), "MB_ENCRYPTION_KEY": strings.Repeat("cd", 32),
		"MB_DATA_DIR": dir, "MB_PUBLIC_URL": "http://bird.test", "MB_ADMIN_ADDR": "127.0.0.1:0",
		"MB_IMAGE_CDN_URL": pds.URL, "MB_PUBLIC_APPVIEW_URL": pds.URL, "MB_INSECURE_ALLOW_PRIVATE_NETWORKS": "true",
		"MB_PER_IP_RATE": "100000", "MB_PER_IP_BURST": "100000", "MB_DEFAULT_HANDLE_HOST": "test",
	}
	for k, v := range env {
		os.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.New(cfg, app.Options{Logger: quiet, Directory: pds.Directory(), PlainHTTPHosts: []string{pdsLn.Addr().String()}})
	if err != nil {
		return nil, err
	}
	defer a.Close()

	var (
		mu        sync.Mutex
		overheads []time.Duration
		recording atomic.Bool
		routes    = map[string]int64{}
	)
	a.Metrics.Observer = func(route string, code int, total, overhead time.Duration) {
		if !recording.Load() {
			return
		}
		mu.Lock()
		overheads = append(overheads, overhead)
		routes[route]++
		mu.Unlock()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: a.Handler}
	go srv.Serve(ln)
	defer srv.Close()
	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: o.Workers, MaxConnsPerHost: o.Workers}}

	type vu struct {
		auth   string
		sinceH int64
		sinceM int64
		avatar string
		mu     sync.Mutex
	}
	users := make([]*vu, o.Users)
	for i := range users {
		users[i] = &vu{auth: "Basic " + base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("user%d.test:%s", i, password)))}
	}

	var bytesIn atomic.Int64
	do := func(u *vu, path string) (int, []byte, error) {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("Authorization", u.auth)
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("User-Agent", "Tweetie/2.0 CFNetwork/459 Darwin/10.0.0d3")
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		var rd io.Reader = resp.Body
		if resp.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(resp.Body)
			if err != nil {
				return resp.StatusCode, nil, err
			}
			rd = zr
		}
		b, _ := io.ReadAll(rd)
		bytesIn.Add(int64(resp.ContentLength))
		return resp.StatusCode, b, nil
	}

	// Warm-up: every user logs in and loads its timeline once, as a client
	// would on launch. Not measured.
	fmt.Fprintf(o.Out, "warming up %d users...\n", o.Users)
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	for _, u := range users {
		wg.Add(1)
		sem <- struct{}{}
		go func(u *vu) {
			defer wg.Done()
			defer func() { <-sem }()
			code, body, err := do(u, "/statuses/home_timeline.json?count=20")
			if err != nil || code != 200 {
				fmt.Fprintf(o.Out, "warm-up request failed: %d %v %.200s\n", code, err, body)
				return
			}
			var sts []struct {
				ID   int64 `json:"id"`
				User struct {
					ProfileImageURL string `json:"profile_image_url"`
				} `json:"user"`
			}
			if err := json.Unmarshal(body, &sts); err != nil {
				fmt.Fprintf(o.Out, "warm-up decode: %v %.300s\n", err, body)
			}
			if len(sts) > 0 {
				u.sinceH = sts[0].ID
				u.avatar = strings.TrimPrefix(sts[0].User.ProfileImageURL, "http://bird.test")
			}
		}(u)
	}
	wg.Wait()
	logins := pds.CallCount("com.atproto.server.createSession")
	callsBefore := totalCalls(pds)

	// Measured phase: open-model arrivals at the target rate.
	fmt.Fprintf(o.Out, "running %v at %.0f req/s...\n", o.Duration, o.RPS)
	recording.Store(true)
	var (
		n, errs atomic.Int64
		latMu   sync.Mutex
		lat     []time.Duration
	)
	inflight := make(chan struct{}, o.Workers)
	start := time.Now()
	interval := time.Duration(float64(time.Second) / o.RPS)
	next := start
	for time.Since(start) < o.Duration {
		next = next.Add(interval)
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		}
		u := users[rand.IntN(len(users))]
		var path string
		switch r := rand.IntN(100); {
		case r < 55:
			u.mu.Lock()
			path = fmt.Sprintf("/statuses/home_timeline.json?since_id=%d&count=50", u.sinceH)
			u.mu.Unlock()
		case r < 75:
			path = "/statuses/mentions.json?count=20"
		case r < 83:
			path = "/1/statuses/home_timeline.xml?count=20"
		case r < 88:
			path = "/account/verify_credentials.json"
		case r < 93:
			path = fmt.Sprintf("/statuses/user_timeline/user%d.test.json?count=20", rand.IntN(len(users)))
		default:
			path = u.avatar
			if path == "" {
				path = "/help/test.json"
			}
		}
		inflight <- struct{}{}
		wg.Add(1)
		go func(u *vu, path string) {
			defer wg.Done()
			defer func() { <-inflight }()
			t0 := time.Now()
			code, body, err := do(u, path)
			d := time.Since(t0)
			n.Add(1)
			if err != nil || code != 200 {
				errs.Add(1)
				return
			}
			latMu.Lock()
			lat = append(lat, d)
			latMu.Unlock()
			if strings.HasPrefix(path, "/statuses/home_timeline.json") {
				var sts []struct {
					ID int64 `json:"id"`
				}
				if json.Unmarshal(body, &sts) == nil && len(sts) > 0 {
					u.mu.Lock()
					u.sinceH = sts[0].ID
					u.mu.Unlock()
				}
			}
		}(u, path)
	}
	wg.Wait()
	elapsed := time.Since(start)
	recording.Store(false)

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if o.HeapProfile != "" {
		if f, err := os.Create(o.HeapProfile); err == nil {
			pprof.WriteHeapProfile(f)
			f.Close()
		}
	}
	wire := bytesIn.Load()
	res := &Result{WireBytes: wire, Requests: n.Load(), Errors: errs.Load(), Duration: elapsed, AchievedRPS: float64(n.Load()) / elapsed.Seconds(),
		UpstreamCalls: totalCalls(pds) - callsBefore, Logins: logins, HeapInuseMB: float64(ms.HeapInuse) / (1 << 20),
		BridgeHeapMB: (float64(ms.HeapInuse) - float64(baseline.HeapInuse)) / (1 << 20), ByRoute: routes}
	res.ClientP50, res.ClientP95, res.ClientP99 = pct(lat)
	mu.Lock()
	res.OverheadP50, res.OverheadP95, res.OverheadP99 = pct(overheads)
	mu.Unlock()
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		rss := float64(ru.Maxrss)
		if runtime.GOOS == "darwin" {
			rss /= 1 << 20 // bytes
		} else {
			rss /= 1 << 10 // kilobytes
		}
		res.MaxRSSMB = rss
	}
	return res, nil
}

func totalCalls(p *fakepds.Server) int {
	p.CallCount("") // takes the lock; Calls read below is a snapshot
	n := 0
	for _, k := range []string{"app.bsky.feed.getTimeline", "app.bsky.notification.listNotifications", "app.bsky.feed.getPosts",
		"app.bsky.actor.getProfile", "app.bsky.actor.getProfiles", "app.bsky.feed.getAuthorFeed"} {
		n += p.CallCount(k)
	}
	return n
}

func pct(ds []time.Duration) (p50, p95, p99 time.Duration) {
	if len(ds) == 0 {
		return
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(q float64) time.Duration { return s[min(len(s)-1, int(q*float64(len(s))))] }
	return at(.50), at(.95), at(.99)
}

// Print writes a human-readable report.
func Print(w io.Writer, o Options, r *Result) {
	fmt.Fprintf(w, "\nmockingbird load test (%s/%s, %d CPUs)\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	fmt.Fprintf(w, "  users %d, target %.0f req/s, simulated upstream latency %v per call\n", o.Users, o.RPS, o.UpstreamLatency)
	fmt.Fprintf(w, "  requests %d in %v (%.1f req/s), errors %d (%.2f%%)\n", r.Requests, r.Duration.Round(time.Millisecond), r.AchievedRPS,
		r.Errors, 100*float64(r.Errors)/float64(max(1, r.Requests)))
	fmt.Fprintf(w, "  client latency      p50 %v  p95 %v  p99 %v\n", r.ClientP50.Round(time.Microsecond), r.ClientP95.Round(time.Microsecond), r.ClientP99.Round(time.Microsecond))
	fmt.Fprintf(w, "  translation overhead p50 %v  p95 %v  p99 %v  (target p95 < 50ms)\n", r.OverheadP50.Round(time.Microsecond), r.OverheadP95.Round(time.Microsecond), r.OverheadP99.Round(time.Microsecond))
	fmt.Fprintf(w, "  upstream calls during run %d (%.2f per request), logins (createSession) %d for %d users\n",
		r.UpstreamCalls, float64(r.UpstreamCalls)/float64(max(1, r.Requests)), r.Logins, o.Users)
	fmt.Fprintf(w, "  response bytes on the wire (gzip): %.1f KB per request, %.0f kbit/s at this rate\n",
		float64(r.WireBytes)/float64(max(1, r.Requests))/1024, float64(r.WireBytes)*8/1000/r.Duration.Seconds())
	fmt.Fprintf(w, "  memory: bridge heap after GC ~%.0f MB (total heap %.0f MB and max RSS %.0f MB include the fake PDS and load generator)\n",
		r.BridgeHeapMB, r.HeapInuseMB, r.MaxRSSMB)
	var names []string
	for k := range r.ByRoute {
		names = append(names, k)
	}
	sort.Strings(names)
	fmt.Fprint(w, "  by route:")
	for _, k := range names {
		fmt.Fprintf(w, " %s=%d", k, r.ByRoute[k])
	}
	fmt.Fprintln(w)
}
