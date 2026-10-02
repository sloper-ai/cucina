// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	"github.com/sloper-ai/cucina/internal/ports"
)

type priceKey struct {
	Type    string
	Windows bool
}

// pricingOutage is how long the provider serves the embedded table after the
// Price List API failed, instead of retrying it on every call (air-gapped installs).
const pricingOutage = 10 * time.Minute

// InstancePrices returns the on-demand hourly price of each instance type in the
// provider's region (Linux, or Windows with the licence included), with vCPU,
// memory and instance-store NVMe size. Answers come from the AWS Price List API
// and are cached for PriceTTL; when the API is unavailable the embedded, dated
// FallbackPrices table is used. Types that cannot be priced are left out of the
// map and reported in an ErrNotFound error alongside the partial result.
func (p *Provider) InstancePrices(ctx context.Context, types []string, windows bool) (map[string]ports.InstancePrice, error) {
	out := make(map[string]ports.InstancePrice, len(types))
	var missing []string
	var lastErr error
	for _, t := range uniq(types) {
		key := priceKey{Type: t, Windows: windows}
		if v, ok := p.prices.GetIfPresent(key); ok {
			out[t] = v
			continue
		}
		v, err := p.fetchPrice(ctx, key)
		if err == nil {
			p.prices.Set(key, v)
			out[t] = v
			continue
		}
		if isContextErr(err) {
			return out, err
		}
		if fb, ok := fallbackPrice(p.opts.Region, key); ok {
			out[t] = fb
			continue
		}
		missing = append(missing, t)
		lastErr = err
	}
	if len(missing) > 0 {
		return out, fmt.Errorf("%w: no %s price for %s in %s: %w", ports.ErrNotFound, osName(windows), strings.Join(missing, ", "), p.opts.Region, lastErr)
	}
	return out, nil
}

func osName(windows bool) string {
	if windows {
		return "Windows"
	}
	return "Linux"
}

var errPricingDown = errors.New("price list API unavailable (serving the embedded table)")

// fetchPrice asks the Price List API for one (type, OS).
func (p *Provider) fetchPrice(ctx context.Context, key priceKey) (ports.InstancePrice, error) {
	if p.pricing == nil {
		return ports.InstancePrice{}, errPricingDown
	}
	p.mu.Lock()
	down := p.clock.Now().Before(p.pricingDownUntil)
	p.mu.Unlock()
	if down {
		return ports.InstancePrice{}, errPricingDown
	}
	if err := p.pricingBucket.Wait(ctx); err != nil {
		return ports.InstancePrice{}, err
	}
	operation := "RunInstances"
	if key.Windows {
		operation = "RunInstances:0002" // Windows, licence included
	}
	match := func(field, value string) pricingtypes.Filter {
		return pricingtypes.Filter{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String(field), Value: aws.String(value)}
	}
	out, err := p.pricing.GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		Filters: []pricingtypes.Filter{
			match("instanceType", key.Type),
			match("regionCode", p.opts.PricingRegionCode),
			match("tenancy", "Shared"),
			match("operatingSystem", osName(key.Windows)),
			match("preInstalledSw", "NA"),
			match("capacitystatus", "Used"),
			match("licenseModel", "No License required"),
			match("operation", operation),
		},
		MaxResults: aws.Int32(10),
	})
	if err != nil {
		if isContextErr(err) {
			return ports.InstancePrice{}, err
		}
		werr := p.apiErr("GetProducts", p.pricingBucket, err)
		if classify(err) != kindInvalid {
			p.mu.Lock()
			p.pricingDownUntil = p.clock.Now().Add(pricingOutage)
			p.mu.Unlock()
		}
		return ports.InstancePrice{}, werr
	}
	var prices []ports.InstancePrice
	for _, doc := range out.PriceList {
		v, err := parseProduct(doc)
		if err != nil {
			p.log.Warn("unparsable price list product", "type", key.Type, "err", err)
			continue
		}
		if v.Type == key.Type {
			v.Windows = key.Windows
			prices = append(prices, v)
		}
	}
	if len(prices) == 0 {
		return ports.InstancePrice{}, fmt.Errorf("%w: price list has no on-demand %s price for %s in %s", ports.ErrNotFound, osName(key.Windows), key.Type, p.opts.PricingRegionCode)
	}
	// Exactly one product should match; if not, take the highest (a conservative estimate).
	slices.SortFunc(prices, func(a, b ports.InstancePrice) int { return -compareFloat(a.USDPerHour, b.USDPerHour) })
	return prices[0], nil
}

func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// priceListProduct is the subset of a Price List product document we read.
type priceListProduct struct {
	Product struct {
		Attributes struct {
			InstanceType string `json:"instanceType"`
			VCPU         string `json:"vcpu"`
			Memory       string `json:"memory"`
			Storage      string `json:"storage"`
		} `json:"attributes"`
	} `json:"product"`
	Terms struct {
		OnDemand map[string]struct {
			PriceDimensions map[string]struct {
				Unit         string            `json:"unit"`
				PricePerUnit map[string]string `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

// parseProduct extracts the hourly on-demand USD price and the size attributes
// from one Price List product document.
func parseProduct(doc string) (ports.InstancePrice, error) {
	var pr priceListProduct
	if err := json.Unmarshal([]byte(doc), &pr); err != nil {
		return ports.InstancePrice{}, err
	}
	a := pr.Product.Attributes
	out := ports.InstancePrice{Type: a.InstanceType, NVMeGiB: parseNVMe(a.Storage)}
	if v, err := strconv.Atoi(strings.TrimSpace(a.VCPU)); err == nil {
		out.VCPU = v
	}
	out.MemoryGiB = parseGiB(a.Memory)
	found := false
	for _, term := range pr.Terms.OnDemand {
		for _, dim := range term.PriceDimensions {
			if dim.Unit != "Hrs" {
				continue
			}
			usd, err := strconv.ParseFloat(dim.PricePerUnit["USD"], 64)
			if err != nil {
				continue
			}
			if !found || usd > out.USDPerHour {
				out.USDPerHour = usd
			}
			found = true
		}
	}
	if !found {
		return ports.InstancePrice{}, fmt.Errorf("product %s has no hourly on-demand USD price", a.InstanceType)
	}
	return out, nil
}

var (
	gibRe  = regexp.MustCompile(`^([\d,.]+)\s*GiB$`)
	nvmeRe = regexp.MustCompile(`(\d+)\s*x\s*([\d,]+)(?:\s*GB)?\s+NVMe`)
)

func parseGiB(s string) float64 {
	m := gibRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
	if err != nil {
		return 0
	}
	return v
}

// parseNVMe returns the total instance-store NVMe size of "2 x 1900 NVMe SSD".
func parseNVMe(s string) int {
	m := nvmeRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, err1 := strconv.Atoi(m[1])
	size, err2 := strconv.Atoi(strings.ReplaceAll(m[2], ",", ""))
	if err1 != nil || err2 != nil {
		return 0
	}
	return n * size
}

// ------------------------------------------------------------- fallback table

//go:embed fallback_prices.json
var fallbackJSON []byte

// FallbackPriceTable is the embedded last-known price list used when the Price
// List API is unreachable (air-gapped installs, unit tests). AsOf dates it.
type FallbackPriceTable struct {
	AsOf    string                     `json:"asOf"`
	Source  string                     `json:"source"`
	Regions map[string][]FallbackPrice `json:"regions"`
}

// FallbackPrice is one row of the embedded table. A zero WindowsUSDPerHour means
// the type has no Windows price (Graviton).
type FallbackPrice struct {
	Type              string  `json:"type"`
	LinuxUSDPerHour   float64 `json:"linux"`
	WindowsUSDPerHour float64 `json:"windows,omitempty"`
	VCPU              int     `json:"vcpu"`
	MemoryGiB         float64 `json:"memoryGiB"`
	NVMeGiB           int     `json:"nvmeGiB,omitempty"`
}

var fallback = sync.OnceValue(func() FallbackPriceTable {
	var t FallbackPriceTable
	if err := json.Unmarshal(fallbackJSON, &t); err != nil {
		panic("ec2: embedded fallback_prices.json: " + err.Error())
	}
	return t
})

// FallbackPrices returns the embedded, dated price table.
func FallbackPrices() FallbackPriceTable { return fallback() }

func fallbackPrice(region string, key priceKey) (ports.InstancePrice, bool) {
	for _, row := range fallback().Regions[region] {
		if row.Type != key.Type {
			continue
		}
		usd := row.LinuxUSDPerHour
		if key.Windows {
			usd = row.WindowsUSDPerHour
		}
		if usd <= 0 || math.IsNaN(usd) {
			return ports.InstancePrice{}, false
		}
		return ports.InstancePrice{Type: row.Type, USDPerHour: usd, VCPU: row.VCPU, MemoryGiB: row.MemoryGiB, NVMeGiB: row.NVMeGiB, Windows: key.Windows}, true
	}
	return ports.InstancePrice{}, false
}
