// SPDX-License-Identifier: FSL-1.1-ALv2

// Package prom is the Prometheus snapshotter of the e2e collectors: a small
// client for the HTTP query API that records every PromQL query it runs
// (§10.4 "record the PromQL used") through a Recorder, so the report can list
// the exact queries behind each number. Queries come from slo/ (the same
// recording rules alerts and canaries use, R-TEST-8f).
package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sample is one instant-vector element or range point.
type Sample struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
	Time   time.Time         `json:"time"`
}

// Series is one range-vector series.
type Series struct {
	Labels map[string]string `json:"labels"`
	Points []Sample          `json:"points"`
}

// Recorder receives each query and a compact rendering of its result.
type Recorder func(query, rangeSpec, result string, at time.Time)

// Client queries one Prometheus.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Record  Recorder
	Now     func() time.Time
}

type apiResponse struct {
	Status    string          `json:"status"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
}

type resultData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) get(ctx context.Context, path string, q url.Values) (*resultData, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	u := strings.TrimSuffix(c.BaseURL, "/") + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("prometheus %s: %s: %w", path, resp.Status, err)
	}
	if ar.Status != "success" {
		return nil, fmt.Errorf("prometheus %s: %s: %s", path, ar.ErrorType, ar.Error)
	}
	var rd resultData
	if err := json.Unmarshal(ar.Data, &rd); err != nil {
		return nil, err
	}
	return &rd, nil
}

func parseValue(v []any) (time.Time, float64, error) {
	if len(v) != 2 {
		return time.Time{}, 0, fmt.Errorf("malformed sample %v", v)
	}
	ts, ok := v[0].(float64)
	if !ok {
		return time.Time{}, 0, fmt.Errorf("malformed timestamp %v", v[0])
	}
	s, ok := v[1].(string)
	if !ok {
		return time.Time{}, 0, fmt.Errorf("malformed value %v", v[1])
	}
	f, err := strconv.ParseFloat(s, 64)
	sec, frac := math.Modf(ts)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC(), f, err
}

// Query runs an instant query at t (zero = now).
func (c *Client) Query(ctx context.Context, q string, t time.Time) ([]Sample, error) {
	if t.IsZero() {
		t = c.now()
	}
	rd, err := c.get(ctx, "/api/v1/query", url.Values{"query": {q}, "time": {strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64)}})
	if err != nil {
		c.record(q, "", "error: "+err.Error(), t)
		return nil, err
	}
	var out []Sample
	switch rd.ResultType {
	case "vector":
		var vs []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		}
		if err := json.Unmarshal(rd.Result, &vs); err != nil {
			return nil, err
		}
		for _, v := range vs {
			ts, f, err := parseValue(v.Value)
			if err != nil {
				return nil, err
			}
			out = append(out, Sample{Labels: v.Metric, Value: f, Time: ts})
		}
	case "scalar":
		var v []any
		if err := json.Unmarshal(rd.Result, &v); err != nil {
			return nil, err
		}
		ts, f, err := parseValue(v)
		if err != nil {
			return nil, err
		}
		out = append(out, Sample{Value: f, Time: ts})
	default:
		return nil, fmt.Errorf("prometheus: unexpected result type %q", rd.ResultType)
	}
	c.record(q, "", renderSamples(out), t)
	return out, nil
}

// Scalar runs an instant query expected to yield one value. ok is false for
// an empty result (no data).
func (c *Client) Scalar(ctx context.Context, q string, t time.Time) (v float64, ok bool, err error) {
	s, err := c.Query(ctx, q, t)
	if err != nil {
		return 0, false, err
	}
	switch len(s) {
	case 0:
		return 0, false, nil
	case 1:
		return s[0].Value, true, nil
	}
	return 0, false, fmt.Errorf("query %q returned %d series, want 1", q, len(s))
}

// Range runs a range query.
func (c *Client) Range(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]Series, error) {
	spec := fmt.Sprintf("[%s, %s] step %s", start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), step)
	rd, err := c.get(ctx, "/api/v1/query_range", url.Values{
		"query": {q},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	})
	if err != nil {
		c.record(q, spec, "error: "+err.Error(), end)
		return nil, err
	}
	var ms []struct {
		Metric map[string]string `json:"metric"`
		Values [][]any           `json:"values"`
	}
	if err := json.Unmarshal(rd.Result, &ms); err != nil {
		return nil, err
	}
	var out []Series
	for _, m := range ms {
		s := Series{Labels: m.Metric}
		for _, v := range m.Values {
			ts, f, err := parseValue(v)
			if err != nil {
				return nil, err
			}
			s.Points = append(s.Points, Sample{Value: f, Time: ts})
		}
		out = append(out, s)
	}
	c.record(q, spec, fmt.Sprintf("%d series", len(out)), end)
	return out, nil
}

func (c *Client) record(q, spec, result string, at time.Time) {
	if c.Record != nil {
		c.Record(q, spec, result, at)
	}
}

func renderSamples(s []Sample) string {
	var b strings.Builder
	for i, x := range s {
		if i == 20 {
			fmt.Fprintf(&b, " … (%d series)", len(s))
			break
		}
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(labelString(x.Labels))
		b.WriteString(" ")
		b.WriteString(strconv.FormatFloat(x.Value, 'g', 6, 64))
	}
	return b.String()
}

func labelString(l map[string]string) string {
	if len(l) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(k + "=" + strconv.Quote(l[k]))
	}
	b.WriteString("}")
	return b.String()
}

// MaxOverRange returns the maximum value of any series in a range result.
func MaxOverRange(ss []Series) (float64, bool) {
	best, ok := math.Inf(-1), false
	for _, s := range ss {
		for _, p := range s.Points {
			if p.Value > best {
				best, ok = p.Value, true
			}
		}
	}
	return best, ok
}
