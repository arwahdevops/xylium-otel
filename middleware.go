// Package xyliumotel provides the OpenTelemetry connector for the Xylium framework.
package xyliumotel

import (
	"fmt"
	"net/http"

	"github.com/arwahdevops/xylium-core/src/xylium"
	"github.com/valyala/fasthttp"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.25.0"
	"go.opentelemetry.io/otel/trace" // Digunakan untuk trace.SpanStartOption, trace.SpanKindServer
)

// MiddlewareConfig holds configuration specific to the OTel middleware operation.
type MiddlewareConfig struct {
	TracerName           string
	SpanNameFormatter    func(c *xylium.Context) string
	AdditionalAttributes []attribute.KeyValue
	Filter               func(c *xylium.Context) bool
}

const defaultMiddlewareTracerName = "xylium.otel.middleware"

// OtelMiddleware returns a Xylium middleware for OpenTelemetry integration,
// using a pre-configured xyliumotel.Connector.
func (connector *Connector) OtelMiddleware(mwCfg ...MiddlewareConfig) xylium.Middleware {
	if connector.IsNoOp() {
		if connector.config.AppLogger != nil { // Cek logger sebelum digunakan
			connector.config.AppLogger.Debug("xylium-otel: Middleware requested, but connector is NoOp. Middleware will be a pass-through.")
		}
		return func(next xylium.HandlerFunc) xylium.HandlerFunc {
			return func(c *xylium.Context) error {
				return next(c)
			}
		}
	}

	cfg := MiddlewareConfig{}
	if len(mwCfg) > 0 {
		cfg = mwCfg[0]
	}

	if cfg.TracerName == "" {
		cfg.TracerName = defaultMiddlewareTracerName
	}
	if cfg.SpanNameFormatter == nil {
		cfg.SpanNameFormatter = func(c *xylium.Context) string {
			path := c.Path()
			if path == "" {
				return "HTTP " + c.Method()
			}
			// Jika Xylium nanti menyediakan c.MatchedRoutePattern():
			// if pattern := c.MatchedRoutePattern(); pattern != "" {
			// 	 return c.Method() + " " + pattern
			// }
			return c.Method() + " " + path
		}
	}

	tracer := connector.GetTracer(cfg.TracerName) // Menggunakan method GetTracer dari Connector
	propagator := connector.Propagator()

	return func(next xylium.HandlerFunc) xylium.HandlerFunc {
		return func(c *xylium.Context) error {
			if cfg.Filter != nil && cfg.Filter(c) {
				if connector.config.AppLogger != nil {
					connector.config.AppLogger.Debugf("xylium-otel: Middleware: Tracing skipped for %s %s due to filter.", c.Method(), c.Path())
				}
				return next(c)
			}

			requestGoCtx := c.GoContext()
			carrier := newFastHTTPHeaderCarrier(&c.Ctx.Request.Header)
			propagatedCtx := propagator.Extract(requestGoCtx, carrier)

			spanName := cfg.SpanNameFormatter(c)
			httpRoute := c.Path() // Ganti dengan c.MatchedRoutePattern() jika tersedia

			attributes := []attribute.KeyValue{
				semconv.HTTPRequestMethodKey.String(c.Method()),
				semconv.URLSchemeKey.String(c.Scheme()),
				semconv.ServerAddressKey.String(c.Host()), // Menggunakan c.Host() untuk server.address
				semconv.URLPathKey.String(c.Path()),
				semconv.HTTPRouteKey.String(httpRoute),
			}
			if queryBytes := c.Ctx.URI().QueryString(); len(queryBytes) > 0 {
				attributes = append(attributes, semconv.URLQueryKey.String(string(queryBytes)))
			}
			if requestIDVal, exists := c.Get(xylium.ContextKeyRequestID); exists {
				if requestID, ok := requestIDVal.(string); ok && requestID != "" {
					attributes = append(attributes, attribute.String("xylium.request_id", requestID))
				}
			}
			if len(cfg.AdditionalAttributes) > 0 {
				attributes = append(attributes, cfg.AdditionalAttributes...)
			}

			spanStartOptions := []trace.SpanStartOption{ // Menggunakan trace.SpanStartOption
				trace.WithAttributes(attributes...),
				trace.WithSpanKind(trace.SpanKindServer), // Menggunakan trace.SpanKindServer
			}

			tracedGoCtx, span := tracer.Start(propagatedCtx, spanName, spanStartOptions...)
			defer span.End()

			spanContext := span.SpanContext()
			if spanContext.HasTraceID() {
				c.Set(xylium.ContextKeyOtelTraceID, spanContext.TraceID().String())
			}
			if spanContext.HasSpanID() {
				c.Set(xylium.ContextKeyOtelSpanID, spanContext.SpanID().String())
			}

			tracedXyliumCtx := c.WithGoContext(tracedGoCtx)
			err := next(tracedXyliumCtx)

			statusCode := c.Ctx.Response.StatusCode()
			span.SetAttributes(semconv.HTTPResponseStatusCodeKey.Int(statusCode))

			if err != nil {
				span.RecordError(err, trace.WithStackTrace(true)) // Menggunakan trace.WithStackTrace
				span.SetStatus(codes.Error, err.Error())
			} else {
				if statusCode >= http.StatusInternalServerError {
					span.SetStatus(codes.Error, fmt.Sprintf("HTTP server error: status code %d", statusCode))
				}
				// Else, status is Unset (implicitly OK)
			}
			return err
		}
	}
}

// fastHTTPHeaderCarrier (sama seperti sebelumnya)
type fastHTTPHeaderCarrier struct {
	header *fasthttp.RequestHeader
}

func newFastHTTPHeaderCarrier(header *fasthttp.RequestHeader) *fastHTTPHeaderCarrier {
	return &fastHTTPHeaderCarrier{header: header}
}
func (fc *fastHTTPHeaderCarrier) Get(key string) string {
	return string(fc.header.Peek(key))
}
func (fc *fastHTTPHeaderCarrier) Set(key string, value string) {
	fc.header.Set(key, value)
}
func (fc *fastHTTPHeaderCarrier) Keys() []string {
	var keys []string
	fc.header.VisitAll(func(key, value []byte) {
		keys = append(keys, string(key))
	})
	return keys
}
