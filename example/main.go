// Package main provides a runnable example demonstrating the use of the xylium-otel connector
// with the Xylium core framework for distributed tracing.
//
// This example application:
// 1. Initializes the Xylium core application.
// 2. Configures and initializes the xylium-otel connector.
//   - It attempts to use an OTLP gRPC exporter if the OTEL_EXPORTER_OTLP_ENDPOINT
//     environment variable is set; otherwise, it defaults to a stdout exporter.
//
// 3. Registers the xylium-otel connector instance with Xylium's application store.
// 4. Applies the OpenTelemetry middleware provided by the connector to instrument incoming HTTP requests.
// 5. Defines several HTTP routes:
//   - A root ("/") endpoint that creates a parent span and a child span.
//   - A "/fast" endpoint for a simple, quick response.
//   - An "/error-test" endpoint that simulates an error and records it on a span.
//
// 6. Starts the Xylium server with graceful shutdown.
//
// To run this example:
//  1. Ensure Xylium core and xylium-otel are correctly pathed in your go.mod.
//  2. (Optional) Set the OTEL_EXPORTER_OTLP_ENDPOINT environment variable to your OTLP collector's
//     gRPC endpoint (e.g., "localhost:4317") if you want to send traces to a collector.
//     If not set, traces will be printed to standard output.
//  3. Run `go run main.go` from the example directory.
//  4. Access the endpoints (e.g., http://localhost:8080/, http://localhost:8080/fast) in your browser or API client.
package main

import (
	// "context" // Removed as it's not directly used in this example file
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/arwahdevops/xylium-core/src/xylium" // Adjust path to your Xylium core
	xyliumotel "github.com/arwahdevops/xylium-otel" // Adjust path to your xylium-otel connector

	"go.opentelemetry.io/otel/attribute"
	// "go.opentelemetry.io/otel/trace" // Not directly used here, but good to be aware of
)

func main() {
	// --- 1. Initialize Xylium App ---
	// Create a new Xylium application instance.
	// The application logger is automatically configured based on Xylium's operating mode.
	app := xylium.New()
	appLogger := app.Logger() // Get the application-level logger.
	appLogger.Info("Xylium OTel example application starting...")

	// --- 2. Configure and Initialize Xylium OTel Connector ---
	// Retrieve OTLP endpoint from environment variable. This is a common way to configure exporters.
	otelExporterEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	serviceName := "xylium-otel-example-service"
	serviceVersion := "v0.1.0"
	environment := "development" // Could also be "production", "staging", etc.

	// Determine the exporter type based on whether the OTLP endpoint is set.
	exporterType := xyliumotel.ExporterStdout // Default to stdout for easy local testing.
	if otelExporterEndpoint != "" {
		exporterType = xyliumotel.ExporterOTLPGRPC
		appLogger.Infof("OTel: OTLP Exporter will be used. Endpoint: %s", otelExporterEndpoint)
	} else {
		appLogger.Info("OTel: OTLP endpoint (OTEL_EXPORTER_OTLP_ENDPOINT) not set. Defaulting to Stdout exporter for OpenTelemetry.")
	}

	// Configure the xylium-otel Connector.
	otelConfig := xyliumotel.Config{
		AppLogger:      appLogger,      // Provide the Xylium application logger to the connector.
		ServiceName:    serviceName,    // Essential for identifying your service in traces.
		ServiceVersion: serviceVersion, // Optional: service version.
		Environment:    environment,    // Optional: deployment environment.
		Exporter:       exporterType,   // Specify which exporter to use (otlp_grpc or stdout).
		OTLP: xyliumotel.OTLPConfig{ // Configuration for the OTLP gRPC exporter.
			Endpoint: otelExporterEndpoint,
			// For local OTLP collectors (like Jaeger all-in-one), insecure might be needed.
			// For production, this should typically be false.
			Insecure: true, // Set to true if your local OTLP collector doesn't use TLS.
			// Headers: map[string]string{"my-header": "my-value"}, // Example custom OTLP headers
			Timeout: 10 * time.Second, // Timeout for OTLP export operations.
		},
		// ManageGlobalProviders: &manageGlobal, // Example: manageGlobal := false;
		// Sampler: sdktrace.TraceIDRatioBased(0.5), // Example: Sample 50% of traces. Default is ParentBased(AlwaysSample)
		ShutdownTimeout: 5 * time.Second, // Timeout for shutting down the OTel SDK components.
	}

	// Create a new instance of the OTel Connector.
	otelConnector, err := xyliumotel.New(otelConfig)
	if err != nil {
		appLogger.Fatalf("Failed to initialize Xylium OTel connector: %v", err)
	}

	// --- 3. Register Connector with Xylium's AppStore ---
	// Storing the connector in Xylium's app store makes it accessible in handlers
	// and allows Xylium to manage its lifecycle (e.g., calling Close() on graceful shutdown
	// if the connector implements io.Closer, which xylium-otel does).
	app.AppSet("otelConnector", otelConnector)
	appLogger.Info("OTel Connector registered with Xylium application store.")

	// --- 4. Apply Middleware ---
	// Apply Xylium's built-in RequestID middleware (good practice, often correlated with traces).
	app.Use(xylium.RequestID())
	// Apply the OpenTelemetry middleware provided by the connector.
	// This will instrument all subsequent routes.
	app.Use(otelConnector.OtelMiddleware(xyliumotel.MiddlewareConfig{
		// Optional: Customize middleware behavior
		// SpanNameFormatter: func(c *xylium.Context) string {
		// 	if pattern := c.MatchedRoutePattern(); pattern != "" { // Hypothetical if Xylium provides this
		// 		return c.Method() + " " + pattern
		// 	}
		// 	return c.Method() + " " + c.Path()
		// },
		// Filter: func(c *xylium.Context) bool {
		// 	return c.Path() == "/health" // Example: skip tracing for /health endpoint
		// },
	}))
	// Apply Xylium's Gzip compression middleware.
	app.Use(xylium.Gzip())
	appLogger.Info("Standard middleware (RequestID, OTel, Gzip) applied.")

	// --- 5. Define Application Routes ---
	app.GET("/", func(c *xylium.Context) error {
		// Retrieve the OTel tracer from the connector.
		// The instrumentation name ("handler.root" here) helps identify the source of spans.
		// tracer := otelConnector.GetTracer("handler.root")

		// Alternative if otelConnector is not readily available (e.g. in a deeply nested function)
		// but you know it's registered:
		otelConnVal, _ := c.AppGet("otelConnector")
		tracer := otelConnVal.(*xyliumotel.Connector).GetTracer("handler.root.fromAppGet")

		// Start a new span for the handler logic. c.GoContext() provides the parent context
		// which is already enriched by the OTel middleware with the server span.
		handlerCtx, span := tracer.Start(c.GoContext(), "rootEndpointHandlerLogic")
		defer span.End() // Always ensure spans are ended.

		// Add custom attributes to the span.
		span.SetAttributes(
			attribute.String("user.agent", c.UserAgent()),
			attribute.String("custom.data", "example_value_for_root"),
		)

		// Simulate some work.
		time.Sleep(15 * time.Millisecond)

		// Create a child span for a sub-operation within this handler.
		_, childSpan := tracer.Start(handlerCtx, "processInternalData")
		time.Sleep(5 * time.Millisecond) // Simulate work for child span.
		childSpan.SetAttributes(attribute.Bool("child.processed", true))
		childSpan.End() // End the child span.

		// Use Xylium's contextual logger. If OTel middleware is active,
		// trace_id and span_id should be automatically included in log fields.
		c.Logger().Infof("Root handler executed successfully for path: %s", c.Path())

		// Prepare response.
		responseData := xylium.M{
			"message":     "Hello from Xylium with integrated OpenTelemetry!",
			"serviceName": serviceName, // For display purposes
		}
		// Add trace and span IDs to response if they exist in context (for easy verification).
		if traceID, ok := c.Get(xylium.ContextKeyOtelTraceID); ok {
			responseData["traceIdFromContext"] = traceID
		}
		if spanID, ok := c.Get(xylium.ContextKeyOtelSpanID); ok {
			responseData["serverSpanIdFromContext"] = spanID
		}
		responseData["otlpEndpointConfigured"] = otelExporterEndpoint

		return c.JSON(http.StatusOK, responseData)
	})

	app.GET("/fast", func(c *xylium.Context) error {
		// This handler is simple and doesn't create additional custom spans.
		// It will still be traced by the OTel middleware.
		c.Logger().Info("Fast handler executed.")
		return c.String(http.StatusOK, "This was a quick response!")
	})

	app.GET("/error-test", func(c *xylium.Context) error {
		otelConnVal, _ := c.AppGet("otelConnector")
		tracer := otelConnVal.(*xyliumotel.Connector).GetTracer("handler.error-test")

		_, span := tracer.Start(c.GoContext(), "errorGeneratingOperation")
		defer span.End()

		simulatedError := errors.New("simulated internal error occurred in /error-test handler")
		// Record the error on the span. This is important for OTel to mark the span as failed.
		span.RecordError(simulatedError)
		// Note: The OTel middleware will also automatically set span status to Error if
		// an error is returned by the handler or if status code is >= 500.

		c.Logger().Errorf("Simulating an error in /error-test: %v", simulatedError)
		// Return a Xylium HTTPError. The OTel middleware will capture this.
		return xylium.NewHTTPError(http.StatusInternalServerError, "An intentional error occurred in /error-test.").WithInternal(simulatedError)
	})

	// --- 6. Start the Xylium Server ---
	listenAddr := ":8080"
	appLogger.Infof("Xylium server with OTel connector starting on http://localhost%s", listenAddr)
	appLogger.Infof("Service Name for OTel traces: %s, Version: %s, Environment: %s", serviceName, serviceVersion, environment)
	if otelExporterEndpoint != "" {
		appLogger.Infof("OTel traces will be sent to OTLP endpoint: %s", otelExporterEndpoint)
	} else {
		appLogger.Info("OTel traces will be printed to STDOUT (no OTLP endpoint specified via OTEL_EXPORTER_OTLP_ENDPOINT).")
	}

	// app.Start() includes graceful shutdown, which will also call Close() on the otelConnector
	// (because it implements io.Closer and was registered with app.AppSet).
	if err := app.Start(listenAddr); err != nil {
		appLogger.Fatalf("Error starting Xylium server: %v", err)
	}

	// This message is logged after the server has successfully shut down gracefully.
	appLogger.Info("Xylium server has shut down gracefully.")
}
