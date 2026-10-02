// SPDX-License-Identifier: FSL-1.1-ALv2

// Package ec2 is the EC2 implementation of ports.Compute (R-POOL-2): it launches
// worker instances on demand with idempotency tokens and tags on every resource,
// terminates them on scale-in (never stops them), sweeps orphaned volumes/ENIs,
// resolves AMIs, drives Windows EC2 Fast Launch and prices instance types from the
// AWS Price List API.
//
// Every AWS call goes through the narrow ec2API/pricingAPI interfaces, guarded by
// client-side token buckets (R-SCALE-6) and the SDK's adaptive retry mode with
// capacity errors made non-retryable, so an InsufficientInstanceCapacity answer
// comes back to the caller immediately (the controller owns backoff, R-SCALE-4).
// Design notes and the IAM actions it needs: docs/dev/ec2-provider.md.
package ec2

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/maypok86/otter/v2"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// pricingEndpointRegion hosts the Price List API (it also exists in eu-central-1
// and ap-south-1; the answers are identical).
const pricingEndpointRegion = "us-east-1"

// Options configures a Provider. The zero value is usable.
type Options struct {
	// Region is the EC2 region. It selects the embedded fallback price table and is
	// the default PricingRegionCode. New defaults it to the aws.Config region.
	Region string
	// ExtraTags are added to every resource the provider creates (for example the
	// campaign tags cucina:env, cucina:run, cucina:expires). Launch request tags win.
	ExtraTags map[string]string
	// OrphanGrace is how long a pool-tagged volume or ENI must have been unattached
	// before ListOrphans reports it (default 5m).
	OrphanGrace time.Duration
	// PricingRegionCode is the Price List "regionCode" attribute (default Region).
	PricingRegionCode string
	// PriceTTL is how long a Price List answer is cached (default 24h).
	PriceTTL time.Duration
	// FastLaunchPollInterval is the DescribeFastLaunchImages polling period (default 15s).
	FastLaunchPollInterval time.Duration
	// Limits are the client-side token buckets (R-SCALE-6).
	Limits Limits
	// Clock defaults to the system clock.
	Clock ports.Clock
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// OnAPIError, if set, is called once per failed AWS operation (after the SDK's
	// retries) — the hook behind cucina_ec2_api_errors_total{op,code}.
	OnAPIError func(op, code string)
}

// Provider implements ports.Compute on EC2.
type Provider struct {
	api     ec2API
	pricing pricingAPI // nil: embedded fallback prices only
	opts    Options
	clock   ports.Clock
	log     *slog.Logger

	runBucket, describeBucket, terminateBucket, mutateBucket, pricingBucket *bucket

	images *otter.Cache[string, imageInfo]             // immutable AMI facts used by Launch
	prices *otter.Cache[priceKey, ports.InstancePrice] // Price List answers (TTL)

	mu               sync.Mutex
	firstSeen        map[string]map[string]time.Time // cluster → orphan candidate → first time seen unattached
	pricingDownUntil time.Time                       // circuit breaker for an unreachable Price List API
}

var _ ports.Compute = (*Provider)(nil)

// New returns a Provider using cfg's credentials for EC2 (cfg.Region) and the Price
// List API (us-east-1). The SDK clients use adaptive retries that never retry
// capacity errors.
func New(cfg aws.Config, opts Options) (*Provider, error) {
	if opts.Region == "" {
		opts.Region = cfg.Region
	}
	if opts.Region == "" {
		return nil, fmt.Errorf("%w: ec2 provider: no region configured", ports.ErrInvalid)
	}
	ec2c := ec2sdk.NewFromConfig(cfg, func(o *ec2sdk.Options) {
		o.Region = opts.Region
		o.Retryer = newRetryer()
	})
	pc := pricing.NewFromConfig(cfg, func(o *pricing.Options) {
		o.Region = pricingEndpointRegion
		o.Retryer = newRetryer()
	})
	return newProvider(ec2c, pc, opts)
}

// newRetryer returns the SDK retryer: adaptive mode (client-side rate limiting on
// throttles, exponential backoff with full jitter), no SDK retry quota (our token
// buckets meter requests), and capacity/quota errors marked non-retryable so ICE
// never hot-loops inside the SDK.
func newRetryer() aws.Retryer {
	return retry.NewAdaptiveMode(func(o *retry.AdaptiveModeOptions) {
		o.StandardOptions = append(o.StandardOptions, func(so *retry.StandardOptions) {
			so.MaxAttempts = 4
			so.MaxBackoff = 20 * time.Second
			so.RateLimiter = ratelimit.None
			so.Retryables = append([]retry.IsErrorRetryable{noRetryOnCapacity{}}, so.Retryables...)
		})
	})
}

// noRetryOnCapacity makes ICE, quota and idempotency errors non-retryable: EC2
// reports InsufficientInstanceCapacity with an HTTP 5xx status, which the standard
// retryer would otherwise retry three times against the same (type, subnet).
type noRetryOnCapacity struct{}

func (noRetryOnCapacity) IsErrorRetryable(err error) aws.Ternary {
	switch classify(err) {
	case kindCapacity, kindQuota, kindIdempotentMismatch, kindTokenTerminated, kindImage, kindInvalid:
		return aws.FalseTernary
	}
	return aws.UnknownTernary
}

func newProvider(api ec2API, pc pricingAPI, opts Options) (*Provider, error) {
	if api == nil {
		return nil, errors.New("ec2 provider: nil EC2 client")
	}
	if opts.OrphanGrace <= 0 {
		opts.OrphanGrace = 5 * time.Minute
	}
	if opts.PriceTTL <= 0 {
		opts.PriceTTL = 24 * time.Hour
	}
	if opts.FastLaunchPollInterval <= 0 {
		opts.FastLaunchPollInterval = 15 * time.Second
	}
	if opts.PricingRegionCode == "" {
		opts.PricingRegionCode = opts.Region
	}
	if err := validateTags(opts.ExtraTags); err != nil {
		return nil, fmt.Errorf("ec2 provider: extra tags: %w", err)
	}
	opts.Limits = opts.Limits.withDefaults()
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	images, err := otter.New(&otter.Options[string, imageInfo]{MaximumSize: 256})
	if err != nil {
		return nil, err
	}
	prices, err := otter.New(&otter.Options[priceKey, ports.InstancePrice]{
		MaximumSize:      4096,
		ExpiryCalculator: otter.ExpiryWriting[priceKey, ports.InstancePrice](opts.PriceTTL),
		Clock:            otterClock{clock},
	})
	if err != nil {
		return nil, err
	}
	return &Provider{
		api:             api,
		pricing:         pc,
		opts:            opts,
		clock:           clock,
		log:             log.With("component", "ec2"),
		runBucket:       newBucket(clock, opts.Limits.RunInstances),
		describeBucket:  newBucket(clock, opts.Limits.Describe),
		terminateBucket: newBucket(clock, opts.Limits.Terminate),
		mutateBucket:    newBucket(clock, opts.Limits.Mutate),
		pricingBucket:   newBucket(clock, opts.Limits.Pricing),
		images:          images,
		prices:          prices,
		firstSeen:       map[string]map[string]time.Time{},
	}, nil
}

// Close releases the price cache's background goroutine.
func (p *Provider) Close() error {
	p.prices.StopAllGoroutines()
	p.images.StopAllGoroutines()
	return nil
}

// apiErr records a failed AWS operation and wraps it with its ports sentinel.
// Throttling drains the operation's bucket (and honours Retry-After).
func (p *Provider) apiErr(op string, b *bucket, err error) error {
	return p.apiErrKind(op, b, classify(err), err)
}

func (p *Provider) apiErrKind(op string, b *bucket, kind errKind, err error) error {
	if isContextErr(err) {
		return fmt.Errorf("ec2 %s: %w", op, err)
	}
	p.noteAPIError(op, err)
	wrapped := wrapKind(op, kind, err)
	var te *ThrottleError
	if b != nil && errors.As(wrapped, &te) {
		b.Throttled(te.RetryAfter)
	}
	return wrapped
}

// noteAPIError feeds the OnAPIError metrics hook and the debug log.
func (p *Provider) noteAPIError(op string, err error) {
	code := errorCode(err)
	if code == "" {
		code = "unknown"
	}
	if p.opts.OnAPIError != nil {
		p.opts.OnAPIError(op, code)
	}
	p.log.Debug("ec2 api error", "op", op, "code", code, "err", err)
}

// ---------------------------------------------------------------- tags

// imdsTagKey is the key alphabet EC2 accepts when instance metadata tags are
// enabled (InstanceMetadataTags=enabled, R-POOL-3).
var imdsTagKey = regexp.MustCompile(`^[A-Za-z0-9+\-=._:@]{1,128}$`)

func validateTags(m map[string]string) error {
	if len(m) > 50 {
		return fmt.Errorf("%w: %d tags (EC2 allows 50 per resource)", ports.ErrInvalid, len(m))
	}
	for k, v := range m {
		if !imdsTagKey.MatchString(k) || k == "." || k == ".." || k == "_index" {
			return fmt.Errorf("%w: tag key %q is not usable with instance metadata tags", ports.ErrInvalid, k)
		}
		if strings.HasPrefix(strings.ToLower(k), "aws:") {
			return fmt.Errorf("%w: tag key %q uses the reserved aws: prefix", ports.ErrInvalid, k)
		}
		if len(v) > 256 {
			return fmt.Errorf("%w: tag %q value longer than 256 characters", ports.ErrInvalid, k)
		}
	}
	return nil
}

// owned reports whether tags mark a resource as created by this controller
// installation: both the cluster and the managed-by tag must match.
func owned(tags map[string]string, cluster string) bool {
	return cluster != "" && tags[domain.TagCluster] == cluster && tags[domain.TagManagedBy] == domain.ManagedByValue
}

func toEC2Tags(m map[string]string) []ec2types.Tag {
	keys := slices.Sorted(maps.Keys(m))
	out := make([]ec2types.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

func fromEC2Tags(ts []ec2types.Tag) map[string]string {
	m := make(map[string]string, len(ts))
	for _, t := range ts {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

func filter(name string, values ...string) ec2types.Filter {
	return ec2types.Filter{Name: aws.String(name), Values: values}
}

// ownerFilters are the filters every inventory call carries (R-SCALE-5/6).
func ownerFilters(cluster string) []ec2types.Filter {
	return []ec2types.Filter{
		filter("tag:"+domain.TagCluster, cluster),
		filter("tag:"+domain.TagManagedBy, domain.ManagedByValue),
	}
}

// chunks splits ids into batches of at most n.
func chunks(ids []string, n int) [][]string {
	var out [][]string
	for len(ids) > n {
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}

// uniq returns the non-empty values of ids in order, without duplicates.
func uniq(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// idBatch is the maximum number of IDs per filter value list (R-SCALE-6).
const idBatch = 200
