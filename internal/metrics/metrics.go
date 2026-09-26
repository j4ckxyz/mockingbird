// Package metrics exposes Prometheus metrics. The handler is mounted only
// on the admin listener (loopback or Tailscale).
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every collector.
type Metrics struct {
	reg       *prometheus.Registry
	requests  *prometheus.CounterVec
	latency   *prometheus.HistogramVec
	overhead  *prometheus.HistogramVec
	upstream  *prometheus.HistogramVec
	logins    *prometheus.CounterVec
	images    *prometheus.CounterVec
	unknown   prometheus.Counter
	respCache *prometheus.CounterVec
	gauges    map[string]prometheus.GaugeFunc

	// Observer, if set, receives every request's timings (load testing).
	Observer func(route string, code int, total, overhead time.Duration)
}

// New registers collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mockingbird_requests_total", Help: "Client requests by route and status.",
		}, []string{"route", "code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "mockingbird_request_seconds", Help: "Total request latency.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route"}),
		overhead: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "mockingbird_translation_seconds", Help: "Request latency excluding time waiting on upstream.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25},
		}, []string{"route"}),
		upstream: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "mockingbird_upstream_seconds", Help: "Upstream XRPC call latency.",
			Buckets: []float64{.025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"nsid", "code"}),
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mockingbird_logins_total", Help: "Login attempts (cache misses) by outcome.",
		}, []string{"outcome"}),
		images: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mockingbird_image_requests_total", Help: "Image proxy outcomes.",
		}, []string{"outcome"}),
		unknown: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mockingbird_unknown_endpoint_total", Help: "Requests for unimplemented endpoints.",
		}),
		respCache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mockingbird_response_cache_total", Help: "Response cache lookups.",
		}, []string{"result"}),
		gauges: map[string]prometheus.GaugeFunc{},
	}
	reg.MustRegister(m.requests, m.latency, m.overhead, m.upstream, m.logins, m.images, m.unknown, m.respCache,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// Handler serves /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Gauge registers a gauge computed on scrape.
func (m *Metrics) Gauge(name, help string, f func() float64) {
	g := prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, f)
	m.reg.MustRegister(g)
	m.gauges[name] = g
}

// ObserveRequest records one client request.
func (m *Metrics) ObserveRequest(route string, code int, total, overhead time.Duration) {
	m.requests.WithLabelValues(route, strconv.Itoa(code)).Inc()
	m.latency.WithLabelValues(route).Observe(total.Seconds())
	if overhead < 0 {
		overhead = 0
	}
	m.overhead.WithLabelValues(route).Observe(overhead.Seconds())
	if m.Observer != nil {
		m.Observer(route, code, total, overhead)
	}
}

// ObserveUpstream records one XRPC call.
func (m *Metrics) ObserveUpstream(nsid string, code int, d time.Duration) {
	m.upstream.WithLabelValues(nsid, strconv.Itoa(code)).Observe(d.Seconds())
}

// Login counts a login outcome.
func (m *Metrics) Login(outcome string) { m.logins.WithLabelValues(outcome).Inc() }

// Image counts an image proxy outcome.
func (m *Metrics) Image(outcome string) { m.images.WithLabelValues(outcome).Inc() }

// Unknown counts an unimplemented endpoint request.
func (m *Metrics) Unknown() { m.unknown.Inc() }

// ResponseCache counts a response cache lookup.
func (m *Metrics) ResponseCache(hit bool) {
	if hit {
		m.respCache.WithLabelValues("hit").Inc()
	} else {
		m.respCache.WithLabelValues("miss").Inc()
	}
}

// Registry exposes the registry (tests).
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }
