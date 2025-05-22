// Package xyliumotel provides the OpenTelemetry connector for the Xylium framework.
// This file contains the OpenTelemetry middleware for instrumenting HTTP requests.
package xyliumotel

import (
	"fmt"
	"net/http" // For HTTP status code constants

	"github.com/arwahdevops/xylium-core/src/xylium"
	"github.com/valyala/fasthttp" // For fasthttp.RequestHeader

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0" // Consistent with otel.go
	"go.opentelemetry.io/otel/trace"
)

// MiddlewareConfig holds configuration options specific to the OpenTelemetry middleware's operation.
// These settings allow customization of how server spans are named, what additional attributes are added,
// and which requests might be filtered out from tracing.
type MiddlewareConfig struct {
	// TracerName is the name of the tracer that will be specifically used by this middleware
	// to create server spans. If empty, a default name like "xylium.otel.middleware" is used.
	// This can be different from the connector's main tracer if desired.
	TracerName string

	// SpanNameFormatter is a function to customize the name of the server span created for each request.
	// The xylium.Context of the current request is passed to this function.
	// For best observability and lower cardinality in tracing backends, it is highly recommended
	// to name spans using the HTTP method and the matched route pattern (e.g., "GET /users/:id").
	// If nil, a default formatter is used which typically combines c.Method() and c.Path().
	// Example: `func(c *xylium.Context) string { return c.Method() + " " + c.MatchedRoutePattern() }` (if available)
	SpanNameFormatter func(c *xylium.Context) string

	// AdditionalAttributes allows adding a list of custom key-value attributes
	// to every server span created by this specific middleware instance.
	// These are added in addition to attributes from the global Connector config.
	AdditionalAttributes []attribute.KeyValue

	// Filter is an optional function to conditionally skip tracing for some requests.
	// If Filter returns true for a given xylium.Context, tracing is bypassed for that request.
	// Useful for excluding health checks, metrics endpoints, etc.
	Filter func(c *xylium.Context) bool
}

// defaultMiddlewareTracerName is the default name used for the tracer within the OTel middleware
// if no specific TracerName is provided in MiddlewareConfig.
const defaultMiddlewareTracerName = "xylium.otel.middleware"

// OtelMiddleware returns a Xylium middleware function for OpenTelemetry HTTP server instrumentation.
// This method is called on an initialized xyliumotel.Connector instance.
// It can optionally take a MiddlewareConfig to customize its behavior. If no config is provided,
// sensible defaults are used for span naming and filtering.
//
// The middleware performs the following:
//  1. Extracts trace context from incoming request headers using the Connector's Propagator.
//  2. Starts a new server span for the request, linking it to an existing trace if context was propagated.
//  3. Sets standard OpenTelemetry semantic attributes for HTTP servers on the span.
//  4. Injects the `trace_id` and `span_id` of the active span into the `xylium.Context` store.
//  5. Propagates the Go `context.Context` (enriched with the active span) to subsequent handlers.
//  6. Records errors from the handler chain on the span and sets the span status accordingly.
//  7. Sets the HTTP response status code as a span attribute.
func (connector *Connector) OtelMiddleware(mwCustomCfg ...MiddlewareConfig) xylium.Middleware {
	if connector.IsNoOp() {
		// If the connector is in NoOp mode (e.g., OTel disabled or failed to initialize),
		// return a pass-through middleware that does nothing related to tracing.
		if connector.config.AppLogger != nil {
			connector.config.AppLogger.Debug("xylium-otel: OtelMiddleware requested, but connector is NoOp. Middleware will be a pass-through.")
		}
		return func(next xylium.HandlerFunc) xylium.HandlerFunc {
			return func(c *xylium.Context) error {
				return next(c)
			}
		}
	}

	// Initialize middleware configuration with defaults, then override with user's custom config if provided.
	cfg := MiddlewareConfig{}
	if len(mwCustomCfg) > 0 {
		cfg = mwCustomCfg[0]
	}

	// Apply defaults to middleware config if not set by user.
	if cfg.TracerName == "" {
		cfg.TracerName = defaultMiddlewareTracerName
	}
	if cfg.SpanNameFormatter == nil {
		cfg.SpanNameFormatter = func(c *xylium.Context) string {
			path := c.Path()
			if path == "" { // Should ideally not happen for a valid request.
				return "HTTP " + c.Method() // Fallback span name
			}
			// Ideal: Use matched route pattern if Xylium core provides it.
			// Example: if pattern := c.MatchedRoutePattern(); pattern != "" {
			// 	 return c.Method() + " " + pattern
			// }
			return c.Method() + " " + path // Current best effort default
		}
	}

	// Get a tracer instance. This uses the connector's GetTracer method, which respects
	// the ManageGlobalProviders setting (i.e., it might use a global tracer or an internal one).
	tracer := connector.GetTracer(cfg.TracerName, trace.WithInstrumentationVersion("xylium-otel-middleware/vNext")) // TODO: Add actual version
	propagator := connector.Propagator()

	// Return the actual Xylium middleware function.
	return func(next xylium.HandlerFunc) xylium.HandlerFunc {
		return func(c *xylium.Context) error {
			// Step 1: Apply filter if configured.
			if cfg.Filter != nil && cfg.Filter(c) {
				if connector.config.AppLogger != nil {
					connector.config.AppLogger.Debugf("xylium-otel: Middleware: Tracing skipped for request %s %s due to filter.", c.Method(), c.Path())
				}
				return next(c) // Bypass tracing and proceed to the next handler.
			}

			// Step 2: Extract trace context from incoming request headers.
			// parentGoCtx is the Go context from the Xylium context BEFORE this middleware modifies it.
			parentGoCtx := c.GoContext()
			carrier := newFastHTTPHeaderCarrier(&c.Ctx.Request.Header)
			// propagatedCtx will contain the parent span context if headers were present.
			propagatedCtx := propagator.Extract(parentGoCtx, carrier)

			// Step 3: Determine span name and prepare attributes.
			spanName := cfg.SpanNameFormatter(c)
			// For http.route, ideally use matched route pattern. c.Path() is a fallback.
			httpRoute := c.Path() // TODO: Replace with c.MatchedRoutePattern() when available in Xylium core.

			// Prepare OpenTelemetry semantic attributes for an HTTP server span.
			attributes := []attribute.KeyValue{
				semconv.HTTPRequestMethodKey.String(c.Method()), // HTTP method (e.g., "GET")
				semconv.URLSchemeKey.String(c.Scheme()),         // URL scheme (e.g., "http", "https")
				semconv.ServerAddressKey.String(c.Host()),       // Logical server address from Host header
				semconv.URLPathKey.String(c.Path()),             // Full request path
				semconv.HTTPRouteKey.String(httpRoute),          // The route that matched (or c.Path() as fallback)
				// Optional: semconv.ClientAddressKey.String(c.RealIP()), // If client IP is reliably determined
			}
			// Add URL query if present.
			if queryBytes := c.Ctx.URI().QueryString(); len(queryBytes) > 0 {
				attributes = append(attributes, semconv.URLQueryKey.String(string(queryBytes)))
			}
			// Add Xylium Request ID as a custom attribute if available (set by Xylium's RequestID middleware).
			if requestIDVal, exists := c.Get(xylium.ContextKeyRequestID); exists {
				if requestID, ok := requestIDVal.(string); ok && requestID != "" {
					attributes = append(attributes, attribute.String("xylium.request_id", requestID))
				}
			}
			// Add any additional custom attributes from the middleware configuration.
			if len(cfg.AdditionalAttributes) > 0 {
				attributes = append(attributes, cfg.AdditionalAttributes...)
			}
			// Add attributes from the main Connector configuration (global to this connector instance).
			if len(connector.config.Environment) > 0 { // Example: deployment.environment
				attributes = append(attributes, semconv.DeploymentEnvironmentKey.String(connector.config.Environment))
			}

			// Define span start options.
			spanStartOptions := []trace.SpanStartOption{
				trace.WithAttributes(attributes...),      // Set initial attributes.
				trace.WithSpanKind(trace.SpanKindServer), // This is a server-side span.
			}

			// Step 4: Start the new server span. `propagatedCtx` is used as the parent context.
			tracedGoCtx, span := tracer.Start(propagatedCtx, spanName, spanStartOptions...)
			defer span.End() // Ensure the span is ended when this function returns.

			// Step 5: Inject trace_id and span_id into Xylium's context store for logging.
			spanContext := span.SpanContext()
			if spanContext.HasTraceID() {
				c.Set(xylium.ContextKeyOtelTraceID, spanContext.TraceID().String())
			}
			if spanContext.HasSpanID() {
				c.Set(xylium.ContextKeyOtelSpanID, spanContext.SpanID().String())
			}

			// Create a new Xylium Context with the OTel-enriched Go context.
			// This ensures `c.GoContext()` in subsequent handlers returns the traced context.
			tracedXyliumCtx := c.WithGoContext(tracedGoCtx)

			// Step 6: Execute the next handler in the chain with the new traced Xylium context.
			err := next(tracedXyliumCtx) // The error returned by the rest of the handler chain.

			// Step 7: After the handler chain has executed, record response information on the span.
			statusCode := c.Ctx.Response.StatusCode()
			span.SetAttributes(semconv.HTTPResponseStatusCodeKey.Int(statusCode))

			// Set span status based on the error returned by the handler chain or the HTTP status code.
			if err != nil {
				// If an error was returned by a handler, record it on the span.
				span.RecordError(err, trace.WithStackTrace(true)) // Include stack trace.
				span.SetStatus(codes.Error, err.Error())          // Mark span status as Error.
			} else {
				// If no Go error from handler, check HTTP status for server-side errors (5xx).
				if statusCode >= http.StatusInternalServerError { // 500 or greater.
					span.SetStatus(codes.Error, fmt.Sprintf("HTTP server error: status code %d", statusCode))
				}
				// For HTTP status codes < 500 (e.g., 2xx success, 4xx client errors) and no Go error,
				// the span status remains `codes.Unset` (which is implicitly OK by OTel convention if no error recorded).
			}

			return err // Return the error (or nil) from the handler chain.
		}
	}
}

// fastHTTPHeaderCarrier adapts fasthttp.RequestHeader to the
// `propagation.TextMapCarrier` interface required by OpenTelemetry propagators
// for extracting trace context from HTTP headers.
type fastHTTPHeaderCarrier struct {
	header *fasthttp.RequestHeader
}

// newFastHTTPHeaderCarrier creates a new carrier for the given fasthttp request header.
func newFastHTTPHeaderCarrier(header *fasthttp.RequestHeader) *fastHTTPHeaderCarrier {
	return &fastHTTPHeaderCarrier{header: header}
}

// Get retrieves a single value from the header for a given key.
// Implements `propagation.TextMapCarrier`.
func (fc *fastHTTPHeaderCarrier) Get(key string) string {
	return string(fc.header.Peek(key))
}

// Set sets a value in the header for a given key.
// Implements `propagation.TextMapCarrier`. Used for injection (though not typically by server-side middleware).
func (fc *fastHTTPHeaderCarrier) Set(key string, value string) {
	fc.header.Set(key, value)
}

// Keys returns a slice of all keys present in the header.
// Implements `propagation.TextMapCarrier`.
func (fc *fastHTTPHeaderCarrier) Keys() []string {
	var keys []string
	fc.header.VisitAll(func(key, value []byte) {
		keys = append(keys, string(key))
	})
	return keys
}
