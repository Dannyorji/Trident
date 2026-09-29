package main

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// captureExporter records what the wrapped exporter was actually asked to
// send, so the tests assert on export decisions rather than on internals.
type captureExporter struct {
	exported []sdktrace.ReadOnlySpan
}

func (c *captureExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	c.exported = append(c.exported, spans...)
	return nil
}

func (c *captureExporter) Shutdown(context.Context) error { return nil }

// A ratio of 0 drops everything, so any span reaching the exporter proves the
// RecordOnly path is what kept it alive rather than head sampling.
func TestAlwaysRecordSamplerNeverDrops(t *testing.T) {
	sampler := newAlwaysRecordSampler(0)
	result := sampler.ShouldSample(sdktrace.SamplingParameters{Name: "any"})
	if result.Decision == sdktrace.Drop {
		t.Fatalf("a dropped span can never be reconsidered at export time; got Drop")
	}
	if result.Decision != sdktrace.RecordOnly {
		t.Fatalf("want RecordOnly so the exporter can inspect the finished span, got %v", result.Decision)
	}
}

func TestAlwaysKeepExporterKeepsErrorsAndSlowSpansOnly(t *testing.T) {
	start := time.Now()

	cases := []struct {
		name string
		span tracetest.SpanStub
		want bool
	}{
		{
			name: "error span is kept even though head sampling dropped it",
			span: tracetest.SpanStub{
				Name:      "failing",
				StartTime: start,
				EndTime:   start.Add(10 * time.Millisecond),
				Status:    sdktrace.Status{Code: codes.Error},
			},
			want: true,
		},
		{
			name: "slow span is kept",
			span: tracetest.SpanStub{
				Name:      "slow",
				StartTime: start,
				EndTime:   start.Add(slowSpanThreshold + time.Millisecond),
			},
			want: true,
		},
		{
			name: "fast successful unsampled span is discarded",
			span: tracetest.SpanStub{
				Name:      "fast",
				StartTime: start,
				EndTime:   start.Add(10 * time.Millisecond),
			},
			want: false,
		},
		{
			// Exactly at the threshold is not "slower than" it. Pinned so the
			// boundary cannot drift silently.
			name: "span exactly at the threshold is discarded",
			span: tracetest.SpanStub{
				Name:      "borderline",
				StartTime: start,
				EndTime:   start.Add(slowSpanThreshold),
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &captureExporter{}
			exporter := newAlwaysKeepExporter(capture)

			snapshots := tracetest.SpanStubs{tc.span}.Snapshots()
			if err := exporter.ExportSpans(context.Background(), snapshots); err != nil {
				t.Fatalf("ExportSpans: %v", err)
			}

			got := len(capture.exported) == 1
			if got != tc.want {
				t.Fatalf("kept=%v, want %v (exported %d spans)", got, tc.want, len(capture.exported))
			}
		})
	}
}
