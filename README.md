# Xylium OpenTelemetry (OTel) Connector (`xylium-otel`)

`xylium-otel` is the official Xylium framework connector for seamless integration with [OpenTelemetry (OTel)](https://opentelemetry.io/), enabling robust distributed tracing for your Xylium web applications. This connector simplifies the setup and usage of OTel, allowing you to gain deep insights into request flows, identify performance bottlenecks, and understand interactions between your microservices.

## Table of Contents

*   [✨ Key Features](#-key-features)
*   [🛠️ Prerequisites](#️-prerequisites)
*   [🚀 Installation](#-installation)
*   [🏁 Basic Usage](#-basic-usage)
    *   [1. Initialize the Connector](#1-initialize-the-connector)
    *   [2. Register with Xylium AppStore](#2-register-with-xylium-appstore)
    *   [3. Apply OTel Middleware](#3-apply-otel-middleware)
    *   [4. Create Custom Spans in Handlers](#4-create-custom-spans-in-handlers)
*   [⚙️ Configuration](#️-configuration)
    *   [`xyliumotel.Config`](#xyliumotelconfig)
    *   [`xyliumotel.MiddlewareConfig`](#xyliumotelmiddlewareconfig)
    *   [Exporter Configuration](#exporter-configuration)
    *   [Managing Global OTel Providers](#managing-global-otel-providers)
*   [📄 Logging Integration](#-logging-integration)
*   [Graceful Shutdown](#graceful-shutdown)
*   [📚 Full Example](#-full-example)
*   [🤝 Contributing](#-contributing)
*   [📜 License](#-license)

## ✨ Key Features

*   **Simplified OTel SDK Setup:** Manages OpenTelemetry TracerProvider, Exporter (OTLP gRPC, Stdout), Sampler, and Propagator initialization.
*   **Automatic HTTP Instrumentation:** Provides Xylium middleware (`Connector.OtelMiddleware()`) to automatically create server spans for incoming requests.
*   **Context Propagation:** Seamlessly integrates with Xylium's `c.GoContext()` and `c.WithGoContext()` for propagating trace context through your application.
*   **Xylium Logger Integration:** Automatically injects `trace_id` and `span_id` into `xylium.Context`, making them available to `c.Logger()` for correlated logging.
*   **Semantic Convention Adherence:** Follows OpenTelemetry semantic conventions for HTTP attributes on spans.
*   **Flexible Configuration:** Offers comprehensive `Config` options for service identification, exporter choice, sampling, and more.
*   **Graceful Shutdown:** Implements `io.Closer`, allowing Xylium to automatically shut down the managed OTel TracerProvider.
*   **External Provider Support:** Allows usage of pre-configured external OpenTelemetry TracerProviders.

## 🛠️ Prerequisites

*   Go (version compatible with Xylium Core, e.g., 1.18+).
*   [Xylium Core framework](https://github.com/arwahdevops/xylium-core) installed in your project.
*   (Optional) An OpenTelemetry Collector (e.g., Jaeger, Grafana Agent, OpenTelemetry Collector Contrib) if using the OTLP exporter.

## 🚀 Installation

1.  Add `xylium-otel` to your project:
    ```bash
    go get github.com/arwahdevops/xylium-otel
    ```

2.  Ensure you have Xylium Core:
    ```bash
    go get github.com/arwahdevops/xylium-core
    ```

    `xylium-otel` depends on several OpenTelemetry SDK packages, which will be fetched automatically as dependencies.

## 🏁 Basic Usage

Here's a quick overview of integrating `xylium-otel` into your Xylium application.

### 1. Initialize the Connector

In your `main()` function or application setup:

```go
package main

import (
	"os"
	"time"

	"github.com/arwahdevops/xylium-core/src/xylium"
	xyliumotel "github.com/arwahdevops/xylium-otel"
)

func main() {
	app := xylium.New()
	appLogger := app.Logger()

	// Determine exporter based on environment (example)
	otelExporterEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	exporterType := xyliumotel.ExporterStdout
	if otelExporterEndpoint != "" {
		exporterType = xyliumotel.ExporterOTLPGRPC
	}

	otelConfig := xyliumotel.Config{
		AppLogger:      appLogger,
		ServiceName:    "my-awesome-service",
		ServiceVersion: "1.0.0",
		Environment:    "production",
		Exporter:       exporterType,
		OTLP: xyliumotel.OTLPConfig{
			Endpoint: otelExporterEndpoint, // e.g., "localhost:4317"
			Insecure: true,                 // Set to false for production OTLP endpoints with TLS
			Timeout:  10 * time.Second,
		},
		// Optional: Default is ParentBased(AlwaysSample())
		// Sampler: sdktrace.TraceIDRatioBased(0.1), // Sample 10% of traces
	}

	otelConnector, err := xyliumotel.New(otelConfig)
	if err != nil {
		appLogger.Fatalf("Failed to initialize Xylium OTel connector: %v", err)
	}

	// ... rest of your application setup ...
}
```

### 2. Register with Xylium AppStore

Store the connector instance in Xylium's application store for easy access in handlers and to enable automatic graceful shutdown.

```go
	// ... after otelConnector is successfully initialized
	app.AppSet("otel", otelConnector) // Key "otel" or any preferred key
```

### 3. Apply OTel Middleware

Add the OTel middleware to your Xylium application globally or to specific route groups.

```go
	// Apply Xylium's RequestID middleware first (recommended for correlation)
	app.Use(xylium.RequestID())

	// Apply the OTel middleware from the connector
	app.Use(otelConnector.OtelMiddleware(xyliumotel.MiddlewareConfig{
		// Optional: Customize middleware
		Filter: func(c *xylium.Context) bool {
			return c.Path() == "/healthz" // Don't trace health checks
		},
	}))
```

### 4. Create Custom Spans in Handlers

Access the tracer within your handlers to create child spans for specific operations.

```go
import (
	"net/http"
	"github.com/arwahdevops/xylium-core/src/xylium"
	xyliumotel "github.com/arwahdevops/xylium-otel"
	"go.opentelemetry.io/otel/attribute"
)

func MyHandler(c *xylium.Context) error {
	// Retrieve the connector from AppStore
	otelConnVal, _ := c.AppGet("otel") // Use the key you registered with
	otelConnector := otelConnVal.(*xyliumotel.Connector)

	// Get a tracer
	tracer := otelConnector.GetTracer("my.handler.instrumentation")

	// c.GoContext() already contains the parent server span from the middleware
	handlerCtx, childSpan := tracer.Start(c.GoContext(), "myCustomOperation")
	defer childSpan.End()

	// Add attributes to your custom span
	childSpan.SetAttributes(attribute.String("custom.key", "custom.value"))

	// ... your handler logic ...
	c.Logger().Info("Custom operation performed within handler.") // Will include trace_id, span_id

	return c.String(http.StatusOK, "Handler executed with custom span.")
}
```

## ⚙️ Configuration

### `xyliumotel.Config`

The `xyliumotel.Config` struct is used when calling `xyliumotel.New()` to initialize the connector.

| Field                       | Type                          | Description                                                                                                                              | Default (if not set)                                     |
| --------------------------- | ----------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------- |
| `AppLogger`                 | `xylium.Logger`               | **Required.** Xylium application logger instance.                                                                                        | -                                                        |
| `ServiceName`               | `string`                      | **Required** (if no external provider). Logical name of your service (e.g., "user-service").                                             | -                                                        |
| `ServiceVersion`            | `string`                      | Optional. Version of your service (e.g., "v1.2.3").                                                                                      | ""                                                       |
| `Environment`               | `string`                      | Optional. Deployment environment (e.g., "production", "staging").                                                                        | ""                                                       |
| `Exporter`                  | `ExporterType`                | Type of exporter to use (`ExporterOTLPGRPC`, `ExporterStdout`, `ExporterNone`).                                                          | `ExporterStdout` (Debug/Test mode), `ExporterNone` (Release mode) |
| `OTLP`                      | `OTLPConfig`                  | Configuration for OTLP gRPC exporter.                                                                                                    | See `OTLPConfig` defaults below.                         |
| `ExternalTracerProvider`    | `trace.TracerProvider`        | Optional. Use a pre-configured OTel `trace.TracerProvider`. Connector won't manage its lifecycle.                                        | `nil`                                                    |
| `ExternalSDKTracerProvider` | `*sdktrace.TracerProvider`    | Optional. Use a pre-configured OTel `*sdktrace.TracerProvider`. Takes precedence over `ExternalTracerProvider`.                            | `nil`                                                    |
| `ManageGlobalProviders`     | `*bool`                       | If `true` (default), connector sets global OTel provider/propagator. If `false`, app manages globals.                                  | `true`                                                   |
| `Propagator`                | `propagation.TextMapPropagator` | Optional. OTel propagator.                                                                                                               | `propagation.TraceContext{}` & `propagation.Baggage{}`    |
| `Sampler`                   | `sdktrace.Sampler`            | Optional. OTel sampling strategy.                                                                                                        | `sdktrace.ParentBased(sdktrace.AlwaysSample())`          |
| `ShutdownTimeout`           | `time.Duration`               | Timeout for graceful shutdown of the managed TracerProvider.                                                                             | `5 * time.Second`                                        |
| `Disabled`                  | `bool`                        | If `true`, disables OTel integration; connector becomes NoOp.                                                                            | `false`                                                  |

**`OTLPConfig` Defaults:**
*   `Insecure`: `false`
*   `Timeout`: `10 * time.Second`

### `xyliumotel.MiddlewareConfig`

The `xyliumotel.MiddlewareConfig` struct is optional and can be passed to `connector.OtelMiddleware()`.

| Field                  | Type                               | Description                                                                                                | Default (if not set)                               |
| ---------------------- | ---------------------------------- | ---------------------------------------------------------------------------------------------------------- | -------------------------------------------------- |
| `TracerName`           | `string`                           | Name for the tracer used by the middleware itself.                                                         | `"xylium.otel.middleware"`                         |
| `SpanNameFormatter`    | `func(c *xylium.Context) string`   | Function to customize server span names. **Crucial for good cardinality.**                                 | `c.Method() + " " + c.Path()`                      |
| `AdditionalAttributes` | `[]attribute.KeyValue`             | Static attributes to add to all server spans created by this middleware.                                   | `nil`                                              |
| `Filter`               | `func(c *xylium.Context) bool`     | Function to conditionally skip tracing for requests. Return `true` to skip.                                | `nil` (trace all requests)                         |

**Recommendation for `SpanNameFormatter`:**
For optimal tracing, use the HTTP method and the *matched route pattern* (e.g., `GET /api/users/:id`) for span names, not the raw path. If Xylium Core provides a way to get the matched route pattern (e.g., `c.MatchedRoutePattern()`), use that.

```go
// Ideal SpanNameFormatter (if MatchedRoutePattern becomes available in Xylium Core)
// SpanNameFormatter: func(c *xylium.Context) string {
//     pattern := c.MatchedRoutePattern() // Hypothetical
//     if pattern != "" {
//         return c.Method() + " " + pattern
//     }
//     return c.Method() + " " + c.Path() // Fallback
// },
```

### Exporter Configuration

*   **OTLP gRPC (`ExporterOTLPGRPC`):**
    *   Requires `Config.OTLP.Endpoint` to be set (e.g., `"localhost:4317"` for a local collector).
    *   Set `Config.OTLP.Insecure = true` for local collectors not using TLS. For production, ensure TLS is used and set `Insecure = false`.
    *   Optional: `Config.OTLP.Headers` and `Config.OTLP.Timeout`.
*   **Stdout (`ExporterStdout`):**
    *   Traces are printed to standard output in a human-readable format. Useful for local development.
    *   No additional configuration needed beyond selecting this exporter type.
*   **None (`ExporterNone`):**
    *   No exporter is configured by `xylium-otel`. If no `ExternalTracerProvider` is set, tracing will effectively be disabled by this connector instance unless a global provider is configured elsewhere and `ManageGlobalProviders` is false.

### Managing Global OTel Providers

By default (`Config.ManageGlobalProviders` is `true` or not set), `xylium-otel` will call `otel.SetTracerProvider()` and `otel.SetTextMapPropagator()` when `xyliumotel.New()` is invoked with a configuration that results in an internally managed TracerProvider or a custom Propagator.

If your application manages the global OTel providers itself, set `Config.ManageGlobalProviders = &manageGlobalFalse` (where `manageGlobalFalse := false`). In this scenario:
*   `xylium-otel` will **not** modify the global OTel state.
*   The middleware and `connector.GetTracer()` will use the `TracerProvider` and `Propagator` instances that were either provided externally in `Config` or initialized internally by the connector (but not set globally).
*   You are responsible for ensuring that the global OTel providers (if needed by other parts of your app) are configured correctly.

## 📄 Logging Integration

When the `xylium-otel` middleware is active:
*   The `trace_id` and `span_id` of the current server span are automatically injected into the `xylium.Context` store.
*   `c.Logger()` (Xylium's contextual logger) will automatically pick up these IDs and include them in your structured logs, enabling easy correlation between logs and traces.

Example log output (JSON format) with `c.Logger()`:
```json
{
    "timestamp": "2023-10-28T10:00:00.123Z",
    "level": "INFO",
    "message": "Handler logic executed.",
    "fields": {
        "xylium_request_id": "uuid-abc-123",
        "otel_trace_id": "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4",
        "otel_span_id": "0123456789abcdef"
        // ... other custom log fields ...
    }
}
```

## Graceful Shutdown

The `xyliumotel.Connector` implements the `io.Closer` interface.
*   If you register the `Connector` instance with Xylium's application store using `app.AppSet("key", otelConnector)`, Xylium's graceful shutdown mechanism will automatically call `otelConnector.Close()`.
*   The `Close()` method will shut down the internally managed `TracerProvider` (if one was created by this connector), flushing any pending traces. This respects the `Config.ShutdownTimeout`.
*   If an `ExternalTracerProvider` was supplied in the `Config`, `otelConnector.Close()` will be a no-op for the provider's lifecycle (as the application is responsible for managing it).

## 📚 Full Example

For a runnable example demonstrating initialization, middleware usage, and custom span creation, please see the [`example/main.go`](./example/main.go) file in this repository.

## 🤝 Contributing

Contributions are welcome! Please open an issue to discuss bugs or feature proposals, or submit a pull request.

## 📜 License

`xylium-otel` is licensed under the [MIT License](LICENSE).
