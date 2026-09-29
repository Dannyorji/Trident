package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// slowSpanThreshold marks a span as "slow" for sampling purposes regardless
// of the configured ratio (Issue #457): a low fixed sample rate means the
// one failing/slow request we actually need is almost never the one head
// sampling happened to pick.
const slowSpanThreshold = 2 * time.Second

// recordOnlySampler wraps a ratio-based root sampler but never fully drops a
// span: spans the ratio sampler would drop are still recorded (RecordOnly),
// so alwaysKeepExporter below gets a chance to inspect the finished span
// and force-export it if it turned out to be an error or slow request that
// head sampling couldn't have known about at span-start time.
type recordOnlySampler struct {
	ratio sdktrace.Sampler
}

func newAlwaysRecordSampler(samplingRatio float64) sdktrace.Sampler {
	return &recordOnlySampler{ratio: sdktrace.TraceIDRatioBased(samplingRatio)}
}

func (s *recordOnlySampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	result := s.ratio.ShouldSample(p)
	if result.Decision == sdktrace.Drop {
		result.Decision = sdktrace.RecordOnly
	}
	return result
}

func (s *recordOnlySampler) Description() string {
	return "TridentAlwaysRecordSampler{" + s.ratio.Description() + "}"
}

// alwaysKeepExporter wraps the real OTLP exporter and only forwards spans
// that were either selected by head sampling, or turned out to be an error
// or slower than slowSpanThreshold — the two cases we can't afford to have
// silently dropped by a fixed low sampling ratio.
type alwaysKeepExporter struct {
	sdktrace.SpanExporter
}

func newAlwaysKeepExporter(underlying sdktrace.SpanExporter) sdktrace.SpanExporter {
	return &alwaysKeepExporter{SpanExporter: underlying}
}

func (e *alwaysKeepExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	kept := make([]sdktrace.ReadOnlySpan, 0, len(spans))
	for _, s := range spans {
		if s.SpanContext().IsSampled() ||
			s.Status().Code == codes.Error ||
			s.EndTime().Sub(s.StartTime()) > slowSpanThreshold {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return e.SpanExporter.ExportSpans(ctx, kept)
}
