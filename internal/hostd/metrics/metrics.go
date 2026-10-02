// SPDX-License-Identifier: FSL-1.1-ALv2

// Package metrics is hostd's Prometheus registry (docs/contracts.md §6:
// cucina_hostd_vms{state}, cucina_hostd_l2_requests_total{result},
// cucina_hostd_wan_bytes_total{direction}, cucina_hostd_l2_size_bytes,
// cucina_hostd_disk_free_bytes) plus link, relay and invariant metrics and the
// Go/process collectors (RSS for NFR-M2).
package metrics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Source supplies current values at scrape time.
type Source struct {
	VMStates       func() map[string]int // state -> count
	L2             func() (hits, misses, wanRecv, wanSent, sizeBytes uint64)
	DiskFree       func() uint64
	Connected      func() bool
	RelayBytes     func() map[string][2]uint64 // relay -> {fromVMs, toVMs}
	InvariantCount func() map[string]uint64
}

var vmStates = []string{"cloning", "starting", "running", "stopping", "stopped", "failed"}

// Registry builds a registry over src.
func Registry(src Source) *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	r.MustRegister(&collector{src: src,
		vms:       prometheus.NewDesc("cucina_hostd_vms", "VMs on this host by state.", []string{"state"}, nil),
		l2req:     prometheus.NewDesc("cucina_hostd_l2_requests_total", "Host L2 CAS reads by result (hit, miss).", []string{"result"}, nil),
		wan:       prometheus.NewDesc("cucina_hostd_wan_bytes_total", "Bytes between the host L2 and the central endpoint.", []string{"direction"}, nil),
		l2size:    prometheus.NewDesc("cucina_hostd_l2_size_bytes", "Configured host L2 cache size.", nil, nil),
		diskFree:  prometheus.NewDesc("cucina_hostd_disk_free_bytes", "Free bytes on the volume holding VMs and the L2.", nil, nil),
		connected: prometheus.NewDesc("cucina_hostd_controller_connected", "1 while the HostService session is up.", nil, nil),
		relay:     prometheus.NewDesc("cucina_hostd_relay_bytes_total", "Bytes relayed for VMs.", []string{"relay", "direction"}, nil),
		inv:       prometheus.NewDesc("cucina_invariant_violations_total", "Invariant violations detected by hostd.", []string{"invariant"}, nil),
	})
	return r
}

type collector struct {
	src                                          Source
	vms, l2req, wan, l2size, diskFree, connected *prometheus.Desc
	relay, inv                                   *prometheus.Desc
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.vms, c.l2req, c.wan, c.l2size, c.diskFree, c.connected, c.relay, c.inv} {
		ch <- d
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	if c.src.VMStates != nil {
		counts := c.src.VMStates()
		for _, s := range vmStates {
			ch <- prometheus.MustNewConstMetric(c.vms, prometheus.GaugeValue, float64(counts[s]), s)
		}
	}
	if c.src.L2 != nil {
		hits, misses, recv, sent, size := c.src.L2()
		ch <- prometheus.MustNewConstMetric(c.l2req, prometheus.CounterValue, float64(hits), "hit")
		ch <- prometheus.MustNewConstMetric(c.l2req, prometheus.CounterValue, float64(misses), "miss")
		ch <- prometheus.MustNewConstMetric(c.wan, prometheus.CounterValue, float64(recv), "received")
		ch <- prometheus.MustNewConstMetric(c.wan, prometheus.CounterValue, float64(sent), "sent")
		ch <- prometheus.MustNewConstMetric(c.l2size, prometheus.GaugeValue, float64(size))
	}
	if c.src.DiskFree != nil {
		ch <- prometheus.MustNewConstMetric(c.diskFree, prometheus.GaugeValue, float64(c.src.DiskFree()))
	}
	if c.src.Connected != nil {
		v := 0.0
		if c.src.Connected() {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(c.connected, prometheus.GaugeValue, v)
	}
	if c.src.RelayBytes != nil {
		for name, b := range c.src.RelayBytes() {
			ch <- prometheus.MustNewConstMetric(c.relay, prometheus.CounterValue, float64(b[0]), name, "from_vms")
			ch <- prometheus.MustNewConstMetric(c.relay, prometheus.CounterValue, float64(b[1]), name, "to_vms")
		}
	}
	if c.src.InvariantCount != nil {
		for name, n := range c.src.InvariantCount() {
			ch <- prometheus.MustNewConstMetric(c.inv, prometheus.CounterValue, float64(n), name)
		}
	}
}

// Serve exposes /metrics on addr until ctx is done.
func Serve(ctx context.Context, addr string, r *prometheus.Registry) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(r, promhttp.HandlerOpts{}))
	mux.HandleFunc("/-/healthy", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
