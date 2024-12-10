// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package protocol // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/statsdreceiver/internal/protocol"

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/lightstep/go-expohisto/structure"
	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	semconv "go.opentelemetry.io/collector/semconv/v1.22.0"
	"go.opentelemetry.io/otel/attribute"
)

var (
	errEmptyMetricName  = errors.New("empty metric name")
	errEmptyMetricValue = errors.New("empty metric value")
)

type (
	MetricType   string // From the statsd line e.g., "c", "g", "h"
	TypeName     string // How humans describe the MetricTypes ("counter", "gauge")
	ObserverType string // How the server will aggregate histogram and timings ("gauge", "summary")
)

const (
	tagMetricType = "metric_type"

	CounterType      MetricType = "c"
	GaugeType        MetricType = "g"
	HistogramType    MetricType = "h"
	TimingType       MetricType = "ms"
	DistributionType MetricType = "d"

	CounterTypeName      TypeName = "counter"
	GaugeTypeName        TypeName = "gauge"
	HistogramTypeName    TypeName = "histogram"
	TimingTypeName       TypeName = "timing"
	TimingAltTypeName    TypeName = "timer"
	DistributionTypeName TypeName = "distribution"

	GaugeObserver     ObserverType = "gauge"
	SummaryObserver   ObserverType = "summary"
	HistogramObserver ObserverType = "histogram"
	DisableObserver   ObserverType = "disabled"

	DefaultObserverType = DisableObserver

	receiverName = "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/statsdreceiver"
)

type TimerHistogramMapping struct {
	StatsdType   TypeName        `mapstructure:"statsd_type"`
	ObserverType ObserverType    `mapstructure:"observer_type"`
	Histogram    HistogramConfig `mapstructure:"histogram"`
	Summary      SummaryConfig   `mapstructure:"summary"`
}

type HistogramConfig struct {
	MaxSize int32 `mapstructure:"max_size"`
}

type SummaryConfig struct {
	Percentiles []float64 `mapstructure:"percentiles"`
}

type ObserverCategory struct {
	method             ObserverType
	histogramConfig    structure.Config
	summaryPercentiles []float64
}

var defaultObserverCategory = ObserverCategory{
	method: DefaultObserverType,
}

// StatsDParser supports the Parse method for parsing StatsD messages with Tags.
type StatsDParser struct {
	instrumentsByAddress    map[netAddr]*instruments
	enableMetricType        bool
	enableSimpleTags        bool
	isMonotonicCounter      bool
	enableIPOnlyAggregation bool
	timerEvents             ObserverCategory
	histogramEvents         ObserverCategory
	lastIntervalTime        time.Time
	BuildInfo               component.BuildInfo
}

type instruments struct {
	addr                   net.Addr
	gauges                 map[statsDMetricDescription]pmetric.ScopeMetrics
	counters               map[statsDMetricDescription]pmetric.ScopeMetrics
	summaries              map[statsDMetricDescription]summaryMetric
	histograms             map[statsDMetricDescription]histogramMetric
	timersAndDistributions []pmetric.ScopeMetrics
}

func newInstruments(addr net.Addr) *instruments {
	return &instruments{
		addr:       addr,
		gauges:     make(map[statsDMetricDescription]pmetric.ScopeMetrics),
		counters:   make(map[statsDMetricDescription]pmetric.ScopeMetrics),
		summaries:  make(map[statsDMetricDescription]summaryMetric),
		histograms: make(map[statsDMetricDescription]histogramMetric),
	}
}

type sampleValue struct {
	value float64
	count float64
}

type summaryMetric struct {
	points      []float64
	weights     []float64
	percentiles []float64
}

type histogramStructure = structure.Histogram[float64]

type histogramMetric struct {
	agg *histogramStructure
}

type statsDMetric struct {
	description statsDMetricDescription
	asFloat     float64
	addition    bool
	unit        string
	sampleRate  float64
	timestamp   uint64
}

type statsDMetricDescription struct {
	name       string
	metricType MetricType
	attrs      attribute.Set
}

func (t MetricType) FullName() TypeName {
	switch t {
	case GaugeType:
		return GaugeTypeName
	case CounterType:
		return CounterTypeName
	case TimingType:
		return TimingTypeName
	case HistogramType:
		return HistogramTypeName
	case DistributionType:
		return DistributionTypeName
	}
	return TypeName(fmt.Sprintf("unknown(%s)", t))
}

func (p *StatsDParser) resetState(when time.Time) {
	p.lastIntervalTime = when
	p.instrumentsByAddress = make(map[netAddr]*instruments)
}

func (p *StatsDParser) Initialize(enableMetricType bool, enableSimpleTags bool, isMonotonicCounter bool, enableIPOnlyAggregation bool, sendTimerHistogram []TimerHistogramMapping) error {
	p.resetState(timeNowFunc())

	p.histogramEvents = defaultObserverCategory
	p.timerEvents = defaultObserverCategory
	p.enableMetricType = enableMetricType
	p.enableSimpleTags = enableSimpleTags
	p.isMonotonicCounter = isMonotonicCounter
	p.enableIPOnlyAggregation = enableIPOnlyAggregation

	// Note: validation occurs in ("../".Config).validate()
	for _, eachMap := range sendTimerHistogram {
		switch eachMap.StatsdType {
		case HistogramTypeName, DistributionTypeName:
			p.histogramEvents.method = eachMap.ObserverType
			p.histogramEvents.histogramConfig = expoHistogramConfig(eachMap.Histogram)
			p.histogramEvents.summaryPercentiles = eachMap.Summary.Percentiles
		case TimingTypeName, TimingAltTypeName:
			p.timerEvents.method = eachMap.ObserverType
			p.timerEvents.histogramConfig = expoHistogramConfig(eachMap.Histogram)
			p.timerEvents.summaryPercentiles = eachMap.Summary.Percentiles
		case CounterTypeName, GaugeTypeName:
		}
	}
	return nil
}

func expoHistogramConfig(opts HistogramConfig) structure.Config {
	var r []structure.Option
	if opts.MaxSize >= structure.MinSize {
		r = append(r, structure.WithMaxSize(opts.MaxSize))
	}
	return structure.NewConfig(r...)
}

// GetMetrics gets the metrics preparing for flushing and reset the state.
func (p *StatsDParser) GetMetrics() []BatchMetrics {
	batchMetrics := make([]BatchMetrics, 0, len(p.instrumentsByAddress))
	now := timeNowFunc()
	for _, instrument := range p.instrumentsByAddress {
		batch := BatchMetrics{
			Info: client.Info{
				Addr: instrument.addr,
			},
			Metrics: pmetric.NewMetrics(),
		}
		rm := batch.Metrics.ResourceMetrics().AppendEmpty()
		for _, metric := range instrument.gauges {
			p.copyMetricAndScope(rm, metric)
		}

		for _, metric := range instrument.timersAndDistributions {
			p.copyMetricAndScope(rm, metric)
		}

		for _, metric := range instrument.counters {
			setTimestampsForCounterMetric(metric, p.lastIntervalTime, now)
			p.copyMetricAndScope(rm, metric)
		}

		for desc, summaryMetric := range instrument.summaries {
			ilm := rm.ScopeMetrics().AppendEmpty()
			p.setVersionAndNameScope(ilm.Scope())
			percentiles := summaryMetric.percentiles
			if len(summaryMetric.percentiles) == 0 {
				percentiles = statsDDefaultPercentiles
			}
			buildSummaryMetric(
				desc,
				summaryMetric,
				p.lastIntervalTime,
				now,
				percentiles,
				ilm,
			)
		}

		for desc, histogramMetric := range instrument.histograms {
			ilm := rm.ScopeMetrics().AppendEmpty()
			p.setVersionAndNameScope(ilm.Scope())

			buildHistogramMetric(
				desc,
				histogramMetric,
				p.lastIntervalTime,
				now,
				ilm,
			)
		}

		batchMetrics = append(batchMetrics, batch)
	}
	p.resetState(now)
	return batchMetrics
}

func (p *StatsDParser) copyMetricAndScope(rm pmetric.ResourceMetrics, metric pmetric.ScopeMetrics) {
	ilm := rm.ScopeMetrics().AppendEmpty()
	metric.CopyTo(ilm)
	p.setVersionAndNameScope(ilm.Scope())
}

func (p *StatsDParser) setVersionAndNameScope(ilm pcommon.InstrumentationScope) {
	ilm.SetVersion(p.BuildInfo.Version)
	ilm.SetName(receiverName)
}

var timeNowFunc = time.Now

func (p *StatsDParser) observerCategoryFor(t MetricType) ObserverCategory {
	switch t {
	case HistogramType, DistributionType:
		return p.histogramEvents
	case TimingType:
		return p.timerEvents
	case CounterType, GaugeType:
	}
	return defaultObserverCategory
}

// Aggregate for each metric line.
func (p *StatsDParser) Aggregate(line string, addr net.Addr) error {
	iter, err := parseMessageToMetrics(line, p.enableMetricType, p.enableSimpleTags)
	if err != nil {
		return err
	}

	addrKey := newNetAddr(addr)
	if p.enableIPOnlyAggregation {
		addrKey = newIPOnlyNetAddr(addr)
	}

	instrument, ok := p.instrumentsByAddress[addrKey]
	if !ok {
		instrument = newInstruments(addr)
		p.instrumentsByAddress[addrKey] = instrument
	}

	for i := 0; i < iter.len(); i++ {
		parseMetric, err := iter.metric(i)
		if err != nil {
			return err
		}
		p.aggregateMetric(instrument, parseMetric)
	}

	return nil
}

func (p *StatsDParser) aggregateMetric(instrument *instruments, parsedMetric statsDMetric) {
	switch parsedMetric.description.metricType {
	case GaugeType:
		_, ok := instrument.gauges[parsedMetric.description]
		if !ok {
			instrument.gauges[parsedMetric.description] = buildGaugeMetric(parsedMetric, timeNowFunc())
		} else {
			if parsedMetric.addition {
				point := instrument.gauges[parsedMetric.description].Metrics().At(0).Gauge().DataPoints().At(0)
				point.SetDoubleValue(point.DoubleValue() + parsedMetric.gaugeValue())
			} else {
				instrument.gauges[parsedMetric.description] = buildGaugeMetric(parsedMetric, timeNowFunc())
			}
		}

	case CounterType:
		_, ok := instrument.counters[parsedMetric.description]
		if !ok {
			instrument.counters[parsedMetric.description] = buildCounterMetric(parsedMetric, p.isMonotonicCounter)
		} else {
			point := instrument.counters[parsedMetric.description].Metrics().At(0).Sum().DataPoints().At(0)
			point.SetIntValue(point.IntValue() + parsedMetric.counterValue())
		}

	case TimingType, HistogramType, DistributionType:
		category := p.observerCategoryFor(parsedMetric.description.metricType)
		switch category.method {
		case GaugeObserver:
			instrument.timersAndDistributions = append(instrument.timersAndDistributions, buildGaugeMetric(parsedMetric, timeNowFunc()))
		case SummaryObserver:
			raw := parsedMetric.sampleValue()
			if existing, ok := instrument.summaries[parsedMetric.description]; !ok {
				instrument.summaries[parsedMetric.description] = summaryMetric{
					points:      []float64{raw.value},
					weights:     []float64{raw.count},
					percentiles: category.summaryPercentiles,
				}
			} else {
				instrument.summaries[parsedMetric.description] = summaryMetric{
					points:      append(existing.points, raw.value),
					weights:     append(existing.weights, raw.count),
					percentiles: category.summaryPercentiles,
				}
			}
		case HistogramObserver:
			raw := parsedMetric.sampleValue()
			var agg *histogramStructure
			if existing, ok := instrument.histograms[parsedMetric.description]; ok {
				agg = existing.agg
			} else {
				agg = new(histogramStructure)
				agg.Init(category.histogramConfig)

				instrument.histograms[parsedMetric.description] = histogramMetric{
					agg: agg,
				}
			}
			agg.UpdateByIncr(
				raw.value,
				uint64(raw.count), // Note! Rounding float64 to uint64 here.
			)

		case DisableObserver:
			// No action.
		}
	}
}

// statsDMetricIter allows alloc-free iteration over statsDMetric values.
// This is helpful to support multiple values in a single statsd metric 
// such as timers, etc.
type statsDMetricIter struct {
	statsDMetricProperties

	// valueStr contains potentially many values separated by ":".
	valueStr string
	valueCount int
}

type statsDMetricProperties struct {
	description statsDMetricDescription
	addition    bool
	unit        string
	sampleRate  float64
	timestamp   uint64
}

func (i statsDMetricIter) len() int {
	return i.valueCount
}

func (i statsDMetricIter) metric(n int) (statsDMetric, error) {
	var (
		curr string
		rest = i.valueStr
	)
	for j := 0; j < i.valueCount; j++ {
		// Cut is alloc free.
		curr, rest, _ = strings.Cut(rest, ":")
		if len(curr) == 0 {
			return statsDMetric{}, fmt.Errorf(
				"empty value for value in values string: n=%d, values=%s", n, i.valueStr)
		}

		if j < n {
			continue
		}

		value, err := strconv.ParseFloat(curr, 64)
		if err != nil {
			return statsDMetric{}, fmt.Errorf(
				"parse metric value string: n=%d, values=%s, curr=%s", n, i.valueStr, curr)
		}


		return statsDMetric{
			description: i.description,
			asFloat:     value,
			addition:    i.addition,
			unit:        i.unit,
			sampleRate:  i.sampleRate,
			timestamp:   i.timestamp,
		}, nil
	}

	return statsDMetric{}, fmt.Errorf(
		"out of range for value for in values string: n=%d, values=%s", n, i.valueStr)
}

func parseMessageToMetrics(
	line string, 
	enableMetricType bool, 
	enableSimpleTags bool,
) (statsDMetricIter, error) {
	result := statsDMetricIter{}

	nameValue, rest, foundName := strings.Cut(line, "|")
	if !foundName {
		return result, fmt.Errorf("invalid message format: %s", line)
	}

	name, valueStr, foundValue := strings.Cut(nameValue, ":")
	if !foundValue {
		return result, fmt.Errorf("invalid <name>:<value> format: %s", nameValue)
	}

	if name == "" {
		return result, errEmptyMetricName
	}
	result.description.name = name
	if valueStr == "" {
		return result, errEmptyMetricValue
	}
	result.valueStr = valueStr
	result.valueCount = strings.Count(valueStr, ":") + 1
	if strings.HasPrefix(valueStr, "-") || strings.HasPrefix(valueStr, "+") {
		result.addition = true
	}

	metricType, additionalParts, _ := strings.Cut(rest, "|")
	inType := MetricType(metricType)
	switch inType {
	case CounterType, GaugeType, HistogramType, TimingType, DistributionType:
		result.description.metricType = inType
	default:
		return result, fmt.Errorf("unsupported metric type: %s", inType)
	}

	var kvs []attribute.KeyValue

	var part string
	part, additionalParts, _ = strings.Cut(additionalParts, "|")
	for ; len(part) > 0; part, additionalParts, _ = strings.Cut(additionalParts, "|") {
		switch {
		case strings.HasPrefix(part, "@"):
			sampleRateStr := strings.TrimPrefix(part, "@")

			f, err := strconv.ParseFloat(sampleRateStr, 64)
			if err != nil {
				return result, fmt.Errorf("parse sample rate: %s", sampleRateStr)
			}

			result.sampleRate = f
		case strings.HasPrefix(part, "#"):
			tagsStr := strings.TrimPrefix(part, "#")

			// handle an empty tag set
			// where the tags part was still sent (some clients do this)
			if len(tagsStr) == 0 {
				continue
			}

			var tagSet string
			tagSet, tagsStr, _ = strings.Cut(tagsStr, ",")
			for ; len(tagSet) > 0; tagSet, tagsStr, _ = strings.Cut(tagsStr, ",") {
				k, v, _ := strings.Cut(tagSet, ":")
				if k == "" {
					return result, fmt.Errorf("invalid tag format: %q", tagSet)
				}

				// support both simple tags (w/o value) and dimension tags (w/ value).
				// dogstatsd notably allows simple tags.
				if v == "" && !enableSimpleTags {
					return result, fmt.Errorf("invalid tag format: %q", tagSet)
				}

				kvs = append(kvs, attribute.String(k, v))
			}
		case strings.HasPrefix(part, "c:"):
			// As per DogStatD protocol v1.2:
			// https://docs.datadoghq.com/developers/dogstatsd/datagram_shell/?tab=metrics#dogstatsd-protocol-v12
			containerID := strings.TrimPrefix(part, "c:")

			if containerID != "" {
				kvs = append(kvs, attribute.String(semconv.AttributeContainerID, containerID))
			}
		case strings.HasPrefix(part, "T"):
			// As per DogStatD protocol v1.3:
			// https://docs.datadoghq.com/developers/dogstatsd/datagram_shell/?tab=metrics#dogstatsd-protocol-v13
			if inType != CounterType && inType != GaugeType {
				return result, errors.New("only GAUGE and COUNT metrics support a timestamp")
			}

			timestampStr := strings.TrimPrefix(part, "T")
			timestampSeconds, err := strconv.ParseUint(timestampStr, 10, 64)
			if err != nil {
				return result, fmt.Errorf("invalid timestamp: %s", timestampStr)
			}

			result.timestamp = timestampSeconds * 1e9 // Convert seconds to nanoseconds
		default:
			return result, fmt.Errorf("unrecognized message part: %s", part)
		}
	}

	// add metric_type dimension for all metrics
	if enableMetricType {
		metricType := string(result.description.metricType.FullName())

		kvs = append(kvs, attribute.String(tagMetricType, metricType))
	}

	if len(kvs) != 0 {
		result.description.attrs = attribute.NewSet(kvs...)
	}

	return result, nil
}

type netAddr struct {
	Network string
	String  string
}

func newNetAddr(addr net.Addr) netAddr {
	return netAddr{addr.Network(), addr.String()}
}

func newIPOnlyNetAddr(addr net.Addr) netAddr {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		// if there is an error, use the original address
		return netAddr{addr.Network(), addr.String()}
	}
	return netAddr{addr.Network(), host}
}
