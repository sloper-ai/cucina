// SPDX-License-Identifier: FSL-1.1-ALv2

package hostlink_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/hostd/identity"
	"github.com/sloper-ai/cucina/internal/hostlink"
	"github.com/sloper-ai/cucina/internal/hostlink/hostlinktest"
	cproto "github.com/sloper-ai/cucina/internal/proto"
)

// Guards: T13 / R-OBS-1 — the controller exposes authenticated outbound host
// counters, not stale or disconnected samples masquerading as current metrics.
func TestHostMetricsExpireAcrossSessions(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, ef)
		collector, ok := any(e.ctrl.Host()).(prometheus.Collector)
		require.True(t, ok, "HostService must expose its authenticated metrics to the controller registry")
		r := prometheus.NewPedanticRegistry()
		require.NoError(t, r.Register(collector))
		absent := func() {
			t.Helper()
			families, err := r.Gather()
			require.NoError(t, err)
			require.Empty(t, families, "unavailable telemetry must be absent, not zero or stale")
		}
		absent()
		// Drive individual heartbeats ourselves over real mTLS; unlike hostd's
		// normal link this client sends no automatic heartbeat after Hello.
		client := metricsClient(t, e.ctrl, serial)
		stream := metricsStream(t, client, serial, nil)
		defer func() { _ = stream.CloseSend() }()
		absent() // Hello is not a metrics sample.
		wanReceived, wanSent := uint64(12288), uint64(1024)
		send := func() {
			require.NoError(t, stream.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_Heartbeat{Heartbeat: &cucinav1.Heartbeat{
				// Freshness is based on controller receive time, not this timestamp.
				Time: timestamppb.New(time.Now().Add(24 * time.Hour)),
				Metrics: &cucinav1.HostMetrics{L2Hits: 11, L2Misses: 7, WanBytesReceived: wanReceived, WanBytesSent: wanSent,
					L2SizeBytes: 1048576, DiskFreeBytes: 2097152},
			}}}))
			synctest.Wait()
		}
		send()
		want := `
# HELP cucina_hostd_disk_free_bytes Free bytes on the volume holding VMs and the L2.
# TYPE cucina_hostd_disk_free_bytes gauge
cucina_hostd_disk_free_bytes{serial="TESTSERIAL01"} 2097152
# HELP cucina_hostd_l2_requests_total Host L2 CAS reads by result (hit, miss).
# TYPE cucina_hostd_l2_requests_total counter
cucina_hostd_l2_requests_total{result="hit",serial="TESTSERIAL01"} 11
cucina_hostd_l2_requests_total{result="miss",serial="TESTSERIAL01"} 7
# HELP cucina_hostd_l2_size_bytes Configured host L2 cache size.
# TYPE cucina_hostd_l2_size_bytes gauge
cucina_hostd_l2_size_bytes{serial="TESTSERIAL01"} 1048576
# HELP cucina_hostd_wan_bytes_total Bytes between the host L2 and the central endpoint.
# TYPE cucina_hostd_wan_bytes_total counter
cucina_hostd_wan_bytes_total{direction="received",serial="TESTSERIAL01"} 12288
cucina_hostd_wan_bytes_total{direction="sent",serial="TESTSERIAL01"} 1024
`
		check := func() {
			t.Helper()
			require.NoError(t, testutil.GatherAndCompare(r, strings.NewReader(want)))
			problems, err := testutil.CollectAndLint(collector)
			require.NoError(t, err)
			require.Empty(t, problems)
		}
		check()
		check() // A scrape must not add the same heartbeat counters again.
		for range 5 {
			<-time.After(10 * time.Second)
			require.NoError(t, stream.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_Log{Log: &cucinav1.LogData{CommandId: "unrelated"}}}))
			synctest.Wait()
		}
		require.True(t, e.online(t)(), "unrelated traffic keeps the session alive")
		absent() // But its metrics heartbeat is more than 45 seconds old.
		send()
		check()
		require.NoError(t, stream.CloseSend())
		synctest.Wait()
		require.False(t, e.online(t)())
		absent()
		stream = metricsStream(t, client, serial, nil)
		require.True(t, e.online(t)())
		absent() // Reconnect cannot resurrect the previous session's sample.
		send()
		check()

		// Protocol 1.0 reported logical blob sizes in these fields. Reconnecting
		// from a current host must not make those legacy values physical bytes.
		reliable, _, _ := strings.Cut(want, "# HELP cucina_hostd_wan_bytes_total")
		wanReceived, wanSent = 100, 100
		for _, tc := range []struct {
			minor    uint32
			physical bool
		}{
			{minor: 0, physical: false},
			{minor: 1, physical: true},
		} {
			require.NoError(t, stream.CloseSend())
			synctest.Wait()
			stream = metricsStream(t, client, serial, nil, tc.minor)
			absent()
			send()
			expected := reliable
			if tc.physical {
				expected = strings.NewReplacer("12288", "100", "1024\n", "100\n").Replace(want)
			}
			require.NoError(t, testutil.GatherAndCompare(r, strings.NewReader(expected)), "minor %d: only actual transport counters may use the physical WAN family", tc.minor)
		}
	})
}

// Guards: R-OBS-1 / T13 — raw metrics are bounded, belong to the authenticated
// host/VM and disappear when invalid, stale or disconnected; labels cannot spoof identity.
func TestMetricsRelayTrustAndFreshness(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, ef)
		provider, ok := any(e.ctrl.Host()).(interface{ MetricsHandler(string) http.Handler })
		require.True(t, ok, "controller must expose per-target host metrics and HTTP discovery")
		handler := provider.MetricsHandler("cucina-test")
		client := metricsClient(t, e.ctrl, serial)
		stream := metricsStream(t, client, serial, []*cucinav1.VMInfo{{Name: "vm-1", Pool: "macos-test", State: "running"}})
		defer func() { _ = stream.CloseSend() }()
		request := func(path string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodGet, "http://controller.test:9090"+path, nil)
			// The accepted socket's address, not a load-balanced Service or any
			// payload label, identifies the pod whose cache serves the target.
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 9090}))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w
		}
		const text = "# TYPE process_resident_memory_bytes gauge\nprocess_resident_memory_bytes 123\n"
		send := func(source cucinav1.MetricsSnapshot_Source, vm, data string) {
			require.NoError(t, stream.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_MetricsSnapshot{
				MetricsSnapshot: &cucinav1.MetricsSnapshot{Source: source, VmName: vm, PrometheusText: []byte(data)},
			}}))
			synctest.Wait()
		}
		const path = "/metrics/hosts/TESTSERIAL01/worker/vm-1"
		require.Equal(t, http.StatusNotFound, request(path).Code)
		send(cucinav1.MetricsSnapshot_SOURCE_WORKER, "vm-1", text)
		w := request(path)
		require.Equal(t, http.StatusOK, w.Code)
		require.Contains(t, w.Body.String(), "process_resident_memory_bytes 123")
		var groups []struct {
			Targets []string          `json:"targets"`
			Labels  map[string]string `json:"labels"`
		}
		w = request("/sd/hosts")
		require.Equal(t, http.StatusOK, w.Code)
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &groups))
		require.Len(t, groups, 1)
		require.Equal(t, []string{"192.0.2.10:9090"}, groups[0].Targets)
		require.Equal(t, map[string]string{"namespace": "cucina-test", "serial": serial, "pool": "macos-test", "node": serial + "/vm-1",
			"cucina_component": "worker", "__metrics_path__": path}, groups[0].Labels)
		other, err := hostlink.New(hostlink.Deps{Certs: e.ctrl.Enroll, Clock: hostlinktest.Clock{}})
		require.NoError(t, err)
		for _, route := range []string{path, "/sd/hosts"} {
			response := httptest.NewRecorder()
			other.MetricsHandler("cucina-test").ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://other-controller.test:9090"+route, nil))
			if route == path {
				require.Equal(t, http.StatusNotFound, response.Code, "a non-owning replica cannot serve this pod's session")
			} else {
				require.Equal(t, "[]", strings.TrimSpace(response.Body.String()))
			}
		}
		for _, tc := range []struct{ name, data string }{
			{"spoofed serial", "process_resident_memory_bytes{serial=\"OTHERHOST\"} 1\n"},
			{"spoofed node", "process_resident_memory_bytes{node=\"other/vm\"} 1\n"},
			{"spoofed pool", "process_resident_memory_bytes{pool=\"privileged\"} 1\n"},
			{"spoofed job", "process_resident_memory_bytes{job=\"controller\"} 1\n"},
			{"spoofed instance", "process_resident_memory_bytes{instance=\"other\"} 1\n"},
			{"spoofed namespace", "process_resident_memory_bytes{namespace=\"other\"} 1\n"},
			{"oversize", strings.Repeat("x", (128<<10)+1)},
			{"malformed", "this is not metrics\n"},
			{"unsupported family", "host_secret_token 1\n"},
			{"duplicate sample", text + "process_resident_memory_bytes 456\n"},
			{"untrusted timestamp", "process_resident_memory_bytes 123 123456789\n"},
			{"oversize label", "process_resident_memory_bytes{stage=\"" + strings.Repeat("x", 257) + "\"} 1\n"},
		} {
			send(cucinav1.MetricsSnapshot_SOURCE_WORKER, "vm-1", text)
			data := tc.data
			if strings.HasPrefix(data, "process_resident_memory_bytes") {
				data = "# TYPE process_resident_memory_bytes gauge\n" + data
			}
			send(cucinav1.MetricsSnapshot_SOURCE_WORKER, "vm-1", data)
			require.Equal(t, http.StatusNotFound, request(path).Code, "%s: reject invalid replacement; do not retain false freshness", tc.name)
		}
		send(cucinav1.MetricsSnapshot_SOURCE_WORKER, "unowned", text)
		require.Equal(t, http.StatusNotFound, request("/metrics/hosts/TESTSERIAL01/worker/unowned").Code)
		send(cucinav1.MetricsSnapshot_SOURCE_HOSTD, "vm-1", text)
		require.Equal(t, http.StatusNotFound, request("/metrics/hosts/TESTSERIAL01/hostd").Code)
		for _, source := range []cucinav1.MetricsSnapshot_Source{cucinav1.MetricsSnapshot_SOURCE_HOSTD, cucinav1.MetricsSnapshot_SOURCE_HOST_L2} {
			send(source, "", text)
		}
		send(cucinav1.MetricsSnapshot_SOURCE_WORKER, "vm-1", text)
		for range 5 {
			<-time.After(10 * time.Second)
			require.NoError(t, stream.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_Log{Log: &cucinav1.LogData{}}}))
			synctest.Wait()
		}
		require.True(t, e.online(t)())
		require.Equal(t, "[]", strings.TrimSpace(request("/sd/hosts").Body.String()))
		require.Equal(t, http.StatusNotFound, request(path).Code)
		send(cucinav1.MetricsSnapshot_SOURCE_WORKER, "vm-1", text)
		require.Equal(t, http.StatusOK, request(path).Code)
		require.NoError(t, stream.CloseSend())
		synctest.Wait()
		require.Equal(t, http.StatusNotFound, request(path).Code)
		require.Equal(t, "[]", strings.TrimSpace(request("/sd/hosts").Body.String()))
		stream = metricsStream(t, client, serial, []*cucinav1.VMInfo{
			{Name: "vm-1", Pool: "macos-test", State: "running"},
			{Name: "vm-2", Pool: "macos-test", State: "running"},
			{Name: "vm-3", Pool: "macos-test", State: "running"},
		})
		for _, vm := range []string{"vm-1", "vm-2", "vm-3"} {
			send(cucinav1.MetricsSnapshot_SOURCE_WORKER, vm, text)
			want := http.StatusOK
			if vm == "vm-3" {
				want = http.StatusNotFound
			}
			require.Equal(t, want, request("/metrics/hosts/TESTSERIAL01/worker/"+vm).Code, "two worker targets is a hard bound")
		}
		send(cucinav1.MetricsSnapshot_SOURCE_HOSTD, "", text)
		send(cucinav1.MetricsSnapshot_SOURCE_HOST_L2, "", text)
		require.NoError(t, json.Unmarshal(request("/sd/hosts").Body.Bytes(), &groups))
		require.Len(t, groups, 4)
	})
}

// Guards: R-OBS-1 — the controller's 16 MiB metric-text budget is global across
// authenticated hosts, and disconnect releases capacity instead of leaking it.
func TestMetricsRelayGlobalCacheBudget(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		ctrl := hostlinktest.NewController(t, hostlinktest.NewNet(), ef)
		handler := ctrl.Host().MetricsHandler("test")
		var data strings.Builder
		data.WriteString("# TYPE go_goroutines gauge\n")
		for i := range 400 {
			fmt.Fprintf(&data, "go_goroutines{stage=\"%03d%s\"} 1\n", i, strings.Repeat("x", 230))
		}
		text := []byte(data.String())
		require.Less(t, len(text), 128<<10)
		var streams []cucinav1.HostService_ConnectClient
		defer func() {
			for _, s := range streams {
				_ = s.CloseSend()
			}
		}()
		total, rejected := 0, false
		var last cucinav1.HostService_ConnectClient
		var lastPath string
		get := func(path string) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://controller.test:9090"+path, nil))
			return w
		}
		for i := 0; i < 100 && !rejected; i++ {
			s := fmt.Sprintf("TESTSERIAL%03d", i)
			last = metricsStream(t, metricsClient(t, ctrl, s), s, nil)
			streams = append(streams, last)
			for _, source := range []cucinav1.MetricsSnapshot_Source{cucinav1.MetricsSnapshot_SOURCE_HOSTD, cucinav1.MetricsSnapshot_SOURCE_HOST_L2} {
				require.NoError(t, last.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_MetricsSnapshot{MetricsSnapshot: &cucinav1.MetricsSnapshot{Source: source, PrometheusText: text}}}))
				synctest.Wait()
				component := "hostd"
				if source == cucinav1.MetricsSnapshot_SOURCE_HOST_L2 {
					component = "host-l2"
				}
				lastPath = "/metrics/hosts/" + s + "/" + component
				w := get(lastPath)
				if w.Code == http.StatusNotFound {
					rejected = true
					break
				}
				require.Equal(t, http.StatusOK, w.Code)
				total += w.Body.Len()
				require.LessOrEqual(t, total, 16<<20, "cache budget must apply across hosts, not per host")
			}
		}
		require.True(t, rejected, "a full global cache must reject new snapshots")
		require.NoError(t, streams[0].CloseSend())
		synctest.Wait()
		source := cucinav1.MetricsSnapshot_SOURCE_HOSTD
		if strings.HasSuffix(lastPath, "/host-l2") {
			source = cucinav1.MetricsSnapshot_SOURCE_HOST_L2
		}
		require.NoError(t, last.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_MetricsSnapshot{MetricsSnapshot: &cucinav1.MetricsSnapshot{Source: source, PrometheusText: text}}}))
		synctest.Wait()
		require.Equal(t, http.StatusOK, get(lastPath).Code, "disconnect must release the old host's cache budget")
	})
}

type metricsTransport func(*http.Request) (*http.Response, error)

func (f metricsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Guards: R-OBS-1 / T13 — hostd actually scrapes and forwards worker and process
// evidence over its outbound link, but failed scrapes never refresh old data.
func TestHostdRelaysOnlyFreshScrapes(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, ef)
		var failed atomic.Bool
		registry := prometheus.NewRegistry()
		g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "go_goroutines", Help: "Synthetic process telemetry."})
		g.Set(3)
		require.NoError(t, registry.Register(g))
		e.metricsGatherer = registry
		e.metricsClient = &http.Client{Transport: metricsTransport(func(*http.Request) (*http.Response, error) {
			code := http.StatusOK
			if failed.Load() {
				code = http.StatusServiceUnavailable
			}
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(
				"# TYPE process_resident_memory_bytes gauge\nprocess_resident_memory_bytes 123\n# TYPE unrelated_family gauge\nunrelated_family 99\n"))}, nil
		})}
		e.start(t)
		defer e.powerOff(t)
		e.ctrl.Approve(t, serial)
		advanceUntil(t, "host online", 2*time.Minute, e.online(t))
		require.NoError(t, e.ctrl.Host().StartVM(t.Context(), serial, startReq("vm-1")))
		handler := e.ctrl.Host().MetricsHandler("cucina-test")
		get := func(path string) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://controller.test:9090"+path, nil))
			return w
		}
		const path = "/metrics/hosts/TESTSERIAL01/worker/vm-1"
		advanceUntil(t, "worker metrics relayed", 2*time.Minute, func() bool { return get(path).Code == http.StatusOK })
		require.Contains(t, get(path).Body.String(), "process_resident_memory_bytes 123")
		require.NotContains(t, get(path).Body.String(), "unrelated_family")
		require.Equal(t, http.StatusOK, get("/metrics/hosts/TESTSERIAL01/hostd").Code)
		failed.Store(true)
		<-time.After(time.Minute)
		synctest.Wait()
		require.True(t, e.online(t)())
		require.Equal(t, http.StatusNotFound, get(path).Code, "failed scrape must not resend the previous sample")
		require.Equal(t, http.StatusOK, get("/metrics/hosts/TESTSERIAL01/hostd").Code, "unrelated host metrics remain fresh")
		failed.Store(false)
		for _, legacyJournal := range []bool{false, true} {
			e.stop(t)
			synctest.Wait()
			require.Equal(t, http.StatusNotFound, get(path).Code)
			if legacyJournal {
				// Upgrade from protocol 1.0: the old journal did not persist ports.
				path := filepath.Join(e.stateDir, "vms.json")
				b, err := os.ReadFile(path)
				require.NoError(t, err)
				var journal map[string]any
				require.NoError(t, json.Unmarshal(b, &journal))
				for _, vm := range journal["vms"].([]any) {
					delete(vm.(map[string]any), "metricsPort")
				}
				b, err = json.Marshal(journal)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, b, 0o600))
			}
			e.start(t)
			advanceUntil(t, "adopted VM metrics after hostd restart", 2*time.Minute, func() bool { return get(path).Code == http.StatusOK })
		}
	})
}

// Guards: NFR-T4/T6 / R-DATA-7 — WAN telemetry measures the transported byte
// stream, not logical blob sizes, while the L2's upstream TLS identity stays intact.
func TestHostdCountsL2TransportBytes(t *testing.T) {
	ef := hostlinktest.NewEnroll(t)
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, ef)
		e.enableL2 = true
		e.wanListener = loopbackListener{e.net.Listen("wan.test:19891")}
		upstream := e.net.Listen("storage.test:8981")
		defer func() { _ = upstream.Close() }()
		e.start(t)
		defer e.powerOff(t)
		e.ctrl.Approve(t, serial)
		advanceUntil(t, "host online", 2*time.Minute, e.online(t))
		var configBytes []byte
		advanceUntil(t, "L2 config written", time.Minute, func() bool {
			b, err := os.ReadFile(filepath.Join(e.stateDir, "l2", "bb_storage.json"))
			configBytes = b
			return err == nil
		})
		var cfg map[string]any
		require.NoError(t, json.Unmarshal(configBytes, &cfg))
		var connection map[string]any
		var find func(any)
		find = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if _, ok := x["address"]; ok {
					connection = x
				}
				for _, child := range x {
					find(child)
				}
			case []any:
				for _, child := range x {
					find(child)
				}
			}
		}
		find(cfg)
		require.Equal(t, "127.0.0.1:19891", connection["address"], "all L2 upstream connections must cross the counted loopback transport")
		require.Equal(t, "storage.test", connection["tls"].(map[string]any)["serverName"], "loopback must not change certificate validation/SNI")
		client, err := e.net.Dial(t.Context(), "wan.test:19891")
		require.NoError(t, err)
		defer func() { _ = client.Close() }()
		server, err := upstream.Accept()
		require.NoError(t, err)
		defer func() { _ = server.Close() }()
		const sent, received = "opaque-tls-request", "opaque-compressed-tls-response"
		_, err = client.Write([]byte(sent))
		require.NoError(t, err)
		b := make([]byte, len(sent))
		_, err = io.ReadFull(server, b)
		require.NoError(t, err)
		require.Equal(t, sent, string(b))
		_, err = server.Write([]byte(received))
		require.NoError(t, err)
		b = make([]byte, len(received))
		_, err = io.ReadFull(client, b)
		require.NoError(t, err)
		require.Equal(t, received, string(b))
		advanceUntil(t, "transport counts in heartbeat", time.Minute, func() bool {
			m := e.ctrl.Host().Metrics(serial)
			return m.GetWanBytesReceived() == uint64(len(received)) && m.GetWanBytesSent() == uint64(len(sent))
		})
	})
}

// A stateful in-memory listener with loopback TCP endpoint metadata, so the
// production admission policy runs unchanged without real sockets or sleeps.
type loopbackListener struct{ net.Listener }

type loopbackConn struct{ net.Conn }

func (loopbackListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 19891}
}
func (l loopbackListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return loopbackConn{c}, nil
}
func (loopbackConn) LocalAddr() net.Addr { return loopbackListener{}.Addr() }
func (loopbackConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 20000}
}

// A CA-issued host certificate crosses the same TLS/URI-SAN authentication as
// hostd; the test drives the wire to control metrics cadence and reconnects.
func metricsClient(t *testing.T, ctrl *hostlinktest.Controller, hostSerial string) cucinav1.HostServiceClient {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	csr, err := identity.CSR(key, hostSerial)
	require.NoError(t, err)
	cert, err := ctrl.Issuer.IssueHost(csr, hostSerial)
	require.NoError(t, err)
	holder := &identity.Holder{}
	require.NoError(t, holder.Set(cert.ChainPEM, cert.BundlePEM, key))
	conn, err := grpc.NewClient("passthrough:///"+hostlinktest.HostAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(holder.ClientTLS(hostlinktest.ServerName))),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return ctrl.Net.Dial(ctx, addr) }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return cucinav1.NewHostServiceClient(conn)
}

func metricsStream(t *testing.T, client cucinav1.HostServiceClient, hostSerial string, vms []*cucinav1.VMInfo, minorOverride ...uint32) cucinav1.HostService_ConnectClient {
	t.Helper()
	minor := cproto.Minor
	if len(minorOverride) != 0 {
		minor = minorOverride[0]
	}
	stream, err := client.Connect(t.Context())
	require.NoError(t, err)
	require.NoError(t, stream.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_Hello{Hello: &cucinav1.Hello{
		SerialNumber: hostSerial, Protocol: &cucinav1.ProtocolVersion{Major: cproto.Major, Minor: minor}, Vms: vms,
	}}}))
	welcome, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, welcome.GetWelcome())
	synctest.Wait()
	return stream
}
