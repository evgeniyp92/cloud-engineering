// podlab is a tiny HTTP service built to make Kubernetes behavior observable.
// Every lesson in the course that needs a workload uses this app, because each
// endpoint exists to prove a specific Kubernetes concept actually happened.
//
// Day 37 version: instrumented with OpenTelemetry tracing.
//   - initTracer() sets up an OTLP/HTTP exporter (endpoint from the standard
//     OTEL_EXPORTER_OTLP_ENDPOINT env var) with service.name/service.version.
//   - the mux is wrapped in otelhttp.NewHandler -> one server span per request,
//     with W3C traceparent propagation handled for free.
//   - instrument() now logs the trace_id, linking every JSON log line to its trace.
//   - handleError starts a manual child span to show the span API.
// Tracing is OFF unless OTEL_EXPORTER_OTLP_ENDPOINT is set, so podlab:v1
// behavior is unchanged when the env var is absent.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	version = envOr("VERSION", "dev")
	color   = envOr("COLOR", "none")

	healthy atomic.Bool

	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "podlab_http_requests_total",
		Help: "Total HTTP requests handled, by path, method and status code.",
	}, []string{"path", "method", "code"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "podlab_http_request_duration_seconds",
		Help:    "HTTP request latency.",
		Buckets: prometheus.DefBuckets,
	}, []string{"path"})

	buildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "podlab_build_info",
		Help: "Build metadata; value is always 1.",
	}, []string{"version", "color"})
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// initTracer wires the OpenTelemetry SDK: an OTLP/HTTP exporter (reads the
// standard OTEL_EXPORTER_OTLP_ENDPOINT env var; an http:// scheme implies
// insecure, which is what we want in-cluster), a Resource identifying this
// service, a batching span processor, and W3C trace-context propagation.
// Returns a shutdown func that flushes buffered spans.
func initTracer(ctx context.Context) (func(context.Context) error, error) {
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	// The Resource is WHO is emitting: every span from this process carries
	// these attributes. service.name is how you find podlab in Tempo.
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("podlab"),
		semconv.ServiceVersion(version),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter), // batch + async export; never blocks a request
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// Propagation: read/write the W3C `traceparent` header so spans join
	// traces started by callers, and callers we invoke can join ours.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// instrument wraps a handler with logging and Prometheus metrics.
// Day 37: if a span is active (otelhttp put it in the request context),
// the log line carries its trace_id — the metrics->logs->traces link.
func instrument(path string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next(rec, r)
		elapsed := time.Since(start)
		httpRequests.WithLabelValues(path, r.Method, strconv.Itoa(rec.code)).Inc()
		httpDuration.WithLabelValues(path).Observe(elapsed.Seconds())

		attrs := []any{
			"path", r.URL.Path,
			"method", r.Method,
			"status", rec.code,
			"duration_ms", elapsed.Milliseconds(),
			"remote", r.RemoteAddr,
			"version", version,
			"color", color,
		}
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
			attrs = append(attrs, "trace_id", sc.TraceID().String())
		}
		slog.Info("request", attrs...)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// GET / — identity banner: who am I, where am I running, which version/color.
func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	hostname, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]any{
		"app":       "podlab",
		"version":   version,
		"color":     color,
		"hostname":  hostname,
		"pod_ip":    os.Getenv("POD_IP"),
		"node_name": os.Getenv("NODE_NAME"),
		"namespace": os.Getenv("POD_NAMESPACE"),
		"time":      time.Now().UTC().Format(time.RFC3339),
	})
}

// GET /config — dumps env vars and the contents of every file under CONFIG_DIR.
// This is how you PROVE a ConfigMap/Secret mount or override actually landed.
func handleConfig(w http.ResponseWriter, r *http.Request) {
	configDir := envOr("CONFIG_DIR", "/etc/podlab")

	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}

	files := map[string]string{}
	filepath.Walk(configDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil // dir may not exist when nothing is mounted; that's fine
		}
		data, err := os.ReadFile(path)
		if err != nil {
			files[path] = fmt.Sprintf("<error: %v>", err)
			return nil
		}
		if len(data) > 4096 {
			data = data[:4096]
		}
		files[path] = string(data)
		return nil
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"config_dir": configDir,
		"files":      files,
		"env":        env,
	})
}

// GET /healthz — 200 when healthy, 503 when toggled off.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	if healthy.Load() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy"})
}

// POST /healthz/toggle — flip health on/off to watch probes do their job.
func handleHealthzToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		return
	}
	now := !healthy.Load()
	healthy.Store(now)
	slog.Warn("health toggled", "healthy", now)
	writeJSON(w, http.StatusOK, map[string]bool{"healthy": now})
}

// GET /load?seconds=n — burn CPU on all cores for n seconds (default 10, max 120).
// Drives HPA and resource-limit demos.
func handleLoad(w http.ResponseWriter, r *http.Request) {
	seconds := 10
	if s := r.URL.Query().Get("seconds"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 120 {
			seconds = n
		}
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	slog.Info("load started", "seconds", seconds)
	go func() {
		for time.Now().Before(deadline) {
			// busy loop; yield occasionally so the scheduler can breathe
			for i := 0; i < 1e7; i++ {
				_ = i * i
			}
		}
		slog.Info("load finished", "seconds", seconds)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"burning_cpu_for_seconds": seconds})
}

// GET /error?rate=0.5 — respond with a 500 at the given probability.
// Used by the canary-analysis lesson to make a "bad" release fail metrics.
// Day 37: starts a manual child span — the dice roll shows up nested inside
// the otelhttp server span, demonstrating the span API and parent/child links.
func handleError(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer("podlab").Start(r.Context(), "roll-the-dice")
	defer span.End()

	rate := 1.0
	if s := r.URL.Query().Get("rate"); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 0 && f <= 1 {
			rate = f
		}
	}
	span.SetAttributes(attribute.Float64("podlab.error_rate", rate))

	if float64(time.Now().UnixNano()%1000)/1000.0 < rate {
		span.SetStatus(codes.Error, "simulated failure")
		slog.Error("simulated failure", "rate", rate)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "simulated failure"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "lucky this time"})
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	healthy.Store(true)
	buildInfo.WithLabelValues(version, color).Set(1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", instrument("/", handleRoot))
	mux.HandleFunc("/config", instrument("/config", handleConfig))
	mux.HandleFunc("/healthz", handleHealthz) // not instrumented: probes would drown real traffic in the logs
	mux.HandleFunc("/healthz/toggle", instrument("/healthz/toggle", handleHealthzToggle))
	mux.HandleFunc("/load", instrument("/load", handleLoad))
	mux.HandleFunc("/error", instrument("/error", handleError))
	mux.Handle("/metrics", promhttp.Handler())

	// Tracing is opt-in via the standard OTel env var, so this image behaves
	// exactly like v1 anywhere the endpoint isn't configured.
	var handler http.Handler = mux
	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" {
		shutdown, err := initTracer(context.Background())
		if err != nil {
			slog.Error("tracing init failed, continuing without traces", "error", err)
		} else {
			defer shutdown(context.Background()) // flush spans on exit
			// One server span per request; skip probe and scrape noise.
			handler = otelhttp.NewHandler(mux, "podlab",
				otelhttp.WithFilter(func(r *http.Request) bool {
					return r.URL.Path != "/healthz" && r.URL.Path != "/metrics"
				}),
			)
			slog.Info("tracing enabled", "endpoint", endpoint)
		}
	}

	port := envOr("PORT", "8080")
	server := &http.Server{Addr: ":" + port, Handler: handler}

	go func() {
		slog.Info("podlab starting", "port", port, "version", version, "color", color)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown: log SIGTERM so the lifecycle lesson can watch it happen.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	sig := <-stop
	slog.Warn("signal received, draining connections", "signal", sig.String())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	slog.Warn("shutdown complete, exiting")
}
