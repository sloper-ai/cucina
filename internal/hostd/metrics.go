// SPDX-License-Identifier: FSL-1.1-ALv2

package hostd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	hostmetrics "github.com/sloper-ai/cucina/internal/hostd/metrics"
	"github.com/sloper-ai/cucina/internal/hostlink/metrictext"
	"github.com/sloper-ai/cucina/internal/ports"
)

type loopbackAdmission struct{}

func (loopbackAdmission) VMForIP(ip netip.Addr) (string, bool) {
	return "host-l2", ip.IsLoopback()
}

// Count the opaque TLS byte stream, including compression and framing, without
// decrypting or retaining payloads. These are TCP payload bytes, not logical
// blob lengths; TCP/IP headers and retransmissions are outside this counter.
func (a *Agent) wanBytes() (received, sent uint64) {
	if a.wan != nil {
		return a.wan.BytesToVMs.Load(), a.wan.BytesFromVMs.Load()
	}
	return 0, 0
}

func (a *Agent) relayMetrics(ctx context.Context, registry prometheus.Gatherer) error {
	client := http.Client{}
	if a.o.MetricsClient != nil {
		client = *a.o.MetricsClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("metrics redirects are forbidden") }
	for {
		if session, supported := a.link.MetricsSession(); supported {
			type target struct {
				source cucinav1.MetricsSnapshot_Source
				vm     string
				addr   string // empty = in-process host registry
			}
			targets := []target{{source: cucinav1.MetricsSnapshot_SOURCE_HOSTD}}
			if a.l2 != nil {
				targets = append(targets, target{source: cucinav1.MetricsSnapshot_SOURCE_HOST_L2, addr: a.o.L2Metrics})
			}
			vms := a.vmm.MetricsTargets()
			if len(vms) > 2 {
				a.log.Error("refusing metrics for more than two running managed VMs")
			} else {
				for _, vm := range vms {
					targets = append(targets, target{source: cucinav1.MetricsSnapshot_SOURCE_WORKER, vm: vm.Name,
						addr: net.JoinHostPort(vm.IP.String(), strconv.FormatUint(uint64(vm.Port), 10))})
				}
			}
			var wg sync.WaitGroup
			for _, t := range targets {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var raw []byte
					var err error
					if t.addr == "" {
						raw, err = registryText(registry)
					} else {
						raw, err = scrapeMetrics(ctx, &client, t.addr)
					}
					if err == nil && t.source == cucinav1.MetricsSnapshot_SOURCE_WORKER {
						// Native Darwin Buildbarn exposition may lack RSS. This is
						// separately named, freshly measured OS evidence, never a
						// synthesized process_resident_memory_bytes family.
						if rss, rssErr := hostmetrics.WorkerResidentMemory(ctx, func(ctx context.Context, c ports.Command) (ports.ExecResult, error) {
							return a.o.Runtime.GuestExec(ctx, a.o.Config.VMNamePrefix+t.vm, c)
						}); rssErr == nil {
							raw = append(raw, []byte(fmt.Sprintf("\n# TYPE cucina_worker_resident_memory_bytes gauge\ncucina_worker_resident_memory_bytes{source=\"guest-ps\"} %d\n", rss))...)
						}
					}
					if err == nil {
						raw, err = metrictext.Filter(t.source, raw)
					}
					if err != nil {
						// Never echo metric data, response bodies or endpoint credentials.
						a.log.Warn("metrics scrape unavailable", "source", t.source.String(), "vm", t.vm, "err", err)
						return
					}
					a.link.SendMetrics(session, &cucinav1.MetricsSnapshot{Source: t.source, VmName: t.vm, PrometheusText: raw})
				}()
			}
			wg.Wait()
		}
		if err := a.o.Clock.Sleep(ctx, 15*time.Second); err != nil {
			return nil
		}
	}
}

func registryText(registry prometheus.Gatherer) ([]byte, error) {
	families, err := registry.Gather()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for _, family := range families {
		if _, err := expfmt.MetricFamilyToText(&b, family); err != nil {
			return nil, err
		}
		if b.Len() > metrictext.MaxScrapeBytes {
			return nil, errors.New("host metrics scrape exceeds input limit")
		}
	}
	return b.Bytes(), nil
}

func scrapeMetrics(ctx context.Context, client *http.Client, addr string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", nil)
	if err != nil {
		return nil, errors.New("invalid local metric endpoint")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("local metric scrape failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics HTTP status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, metrictext.MaxScrapeBytes+1))
	if err != nil {
		return nil, errors.New("reading metric scrape failed")
	}
	if len(b) > metrictext.MaxScrapeBytes {
		return nil, errors.New("metric scrape exceeds input limit")
	}
	return b, nil
}
