// Package xyliumotel provides the OpenTelemetry (OTel) connector for the Xylium framework.
// It simplifies the integration of distributed tracing into Xylium applications by
// managing the OTel SDK setup (TracerProvider, Exporter, Sampler, Propagator)
// and providing middleware for automatic HTTP request instrumentation.
package xyliumotel

import (
	"context"
	"errors"
	"fmt"
	"io" // For io.Closer
	"time"

	"github.com/arwahdevops/xylium-core/src/xylium"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0" // Using a recent semantic conventions version
	"go.opentelemetry.io/otel/trace"
)

// ExporterType defines the type of OpenTelemetry trace exporter to configure.
type ExporterType string

const (
	// ExporterOTLPGRPC configures the OTLP (OpenTelemetry Protocol) gRPC exporter.
	// Requires OTLPConfig.Endpoint to be set.
	ExporterOTLPGRPC ExporterType = "otlp_grpc"
	// ExporterStdout configures an exporter that writes traces to standard output.
	// Useful for local development and debugging.
	ExporterStdout ExporterType = "stdout"
	// ExporterNone indicates that no exporter should be configured by this connector.
	// Tracing will be effectively disabled unless an external TracerProvider is used,
	// or if ManageGlobalProviders is false and a global provider is set elsewhere.
	ExporterNone ExporterType = "none"
)

// OTLPConfig holds configuration specific to the OTLP exporter.
type OTLPConfig struct {
	// Endpoint is the target URL for the OTLP gRPC exporter (e.g., "localhost:4317").
	Endpoint string
	// Insecure determines whether to use an insecure gRPC connection (e.g., for local testing).
	// Defaults to false (secure connection) if not specified and Endpoint is set.
	Insecure bool
	// Headers is a map of additional headers to send with OTLP gRPC requests.
	Headers map[string]string
	// Timeout for OTLP gRPC export operations.
	// Defaults to 10 seconds if not set.
	Timeout time.Duration
}

// Config holds all configuration options for initializing the OpenTelemetry Connector.
type Config struct {
	// AppLogger is the Xylium application logger instance used by the connector for its own logging.
	// This field is required.
	AppLogger xylium.Logger

	// ServiceName is the logical name of the service, e.g., "user-service".
	// Required if not providing ExternalTracerProvider or ExternalSDKTracerProvider.
	// Used to create the OTel resource.
	ServiceName string
	// ServiceVersion is the version of the service, e.g., "v1.2.3". Optional.
	ServiceVersion string
	// Environment is the deployment environment, e.g., "production", "staging". Optional.
	Environment string

	// Exporter defines the type of trace exporter to initialize if an internal
	// TracerProvider is being created.
	// Defaults to ExporterStdout if Xylium mode is Debug/Test, or ExporterNone if Release,
	// unless an external provider is specified.
	Exporter ExporterType
	// OTLP holds configuration for the OTLP gRPC exporter if Exporter is ExporterOTLPGRPC.
	OTLP OTLPConfig

	// ExternalTracerProvider allows providing a pre-configured trace.TracerProvider.
	// If set, the connector will use this provider and will not manage its lifecycle
	// (e.g., it won't call Shutdown on it). Internal exporter configuration is ignored.
	// If ManageGlobalProviders is true, this provider will be set as the global OTel provider.
	ExternalTracerProvider trace.TracerProvider
	// ExternalSDKTracerProvider allows providing a pre-configured *sdktrace.TracerProvider.
	// Similar to ExternalTracerProvider but for the SDK-specific type. Takes precedence over ExternalTracerProvider.
	// If ManageGlobalProviders is true, this provider will be set as the global OTel provider.
	ExternalSDKTracerProvider *sdktrace.TracerProvider

	// ManageGlobalProviders determines if this connector should manage (set) the global
	// OTel TracerProvider and TextMapPropagator using otel.SetTracerProvider and otel.SetTextMapPropagator.
	// If false, the application is responsible for setting global providers if needed.
	// The connector will then use its internally configured/provided tracer and propagator
	// instances for its operations (e.g., middleware).
	// Defaults to true.
	ManageGlobalProviders *bool // Pointer to distinguish between not set (use default true) and explicitly false.

	// Propagator is the OpenTelemetry TextMapPropagator to use.
	// If nil, a default composite propagator (TraceContext and Baggage) is used.
	// If ManageGlobalProviders is true, this propagator will be set as the global OTel propagator.
	Propagator propagation.TextMapPropagator
	// Sampler defines the sampling strategy for traces.
	// If nil, ParentBased(AlwaysSample()) is used as a default.
	Sampler sdktrace.Sampler

	// ShutdownTimeout is the duration to wait for the managed TracerProvider to shut down gracefully.
	// Defaults to 5 seconds. Only applicable if the connector manages the TracerProvider lifecycle.
	ShutdownTimeout time.Duration
	// Disabled, if true, completely disables OpenTelemetry integration by this connector.
	// The connector will operate in a no-op mode.
	Disabled bool
}

// Connector is the Xylium-aware wrapper for OpenTelemetry functionality.
// It manages the TracerProvider, Propagator, and provides middleware for instrumentation.
type Connector struct {
	config         Config
	tracerProvider *sdktrace.TracerProvider // Holds the SDK TracerProvider if managed internally
	tracer         trace.Tracer             // Tracer instance for this connector's middleware/operations
	propagator     propagation.TextMapPropagator
	isNoOp         bool
}

// New creates and initializes a new OpenTelemetry Connector instance based on the provided configuration.
// It sets up the TracerProvider (either by using an external one or initializing one internally
// based on exporter settings), configures the Propagator and Sampler, and potentially
// registers these with the global OpenTelemetry instance if `ManageGlobalProviders` is true.
// Returns the configured Connector or an error if initialization fails.
func New(cfg Config) (*Connector, error) {
	if cfg.Disabled {
		// If AppLogger is available even when disabled, log it.
		if cfg.AppLogger != nil {
			cfg.AppLogger.Info("xylium-otel: OpenTelemetry integration is explicitly disabled by configuration. Connector will be NoOp.")
		} else {
			fmt.Println("[xylium-otel-bootstrap] OpenTelemetry integration is explicitly disabled by configuration. Connector will be NoOp.")
		}
		return &Connector{isNoOp: true, config: cfg}, nil
	}

	// Validate required configurations
	if cfg.AppLogger == nil {
		return nil, errors.New("xylium-otel: Config.AppLogger is required for the OTel connector")
	}
	if cfg.ServiceName == "" && cfg.ExternalTracerProvider == nil && cfg.ExternalSDKTracerProvider == nil {
		return nil, errors.New("xylium-otel: Config.ServiceName is required when not providing an ExternalTracerProvider or ExternalSDKTracerProvider")
	}

	// Apply defaults
	if cfg.Exporter == "" {
		currentMode := xylium.Mode() // Assumes xylium.Mode() is available and gives "debug", "test", or "release"
		if currentMode == xylium.ReleaseMode {
			cfg.Exporter = ExporterNone
		} else {
			cfg.Exporter = ExporterStdout // Default to Stdout for non-release modes for easier local dev
		}
		cfg.AppLogger.Infof("xylium-otel: Config.Exporter not specified, defaulted to '%s' (Xylium mode: '%s').", cfg.Exporter, currentMode)
	}

	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 5 * time.Second
	}
	if cfg.Sampler == nil {
		cfg.Sampler = sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
	if cfg.ManageGlobalProviders == nil {
		manageGlobalDefault := true
		cfg.ManageGlobalProviders = &manageGlobalDefault
	}
	if cfg.OTLP.Timeout <= 0 && cfg.Exporter == ExporterOTLPGRPC {
		cfg.OTLP.Timeout = 10 * time.Second
	}

	c := &Connector{
		config: cfg,
		isNoOp: false, // Assume not NoOp initially
	}

	// Determine TracerProvider
	var actualTracerProvider trace.TracerProvider // This will be the provider used, either global or internal
	if cfg.ExternalSDKTracerProvider != nil {
		cfg.AppLogger.Info("xylium-otel: Using pre-configured external *sdktrace.TracerProvider.")
		actualTracerProvider = cfg.ExternalSDKTracerProvider
		// No internal management of c.tracerProvider, as it's external.
		// Global setting depends on ManageGlobalProviders.
		if *c.config.ManageGlobalProviders {
			otel.SetTracerProvider(cfg.ExternalSDKTracerProvider)
			cfg.AppLogger.Info("xylium-otel: External *sdktrace.TracerProvider set as global OTel provider.")
		}
	} else if cfg.ExternalTracerProvider != nil {
		cfg.AppLogger.Info("xylium-otel: Using pre-configured external trace.TracerProvider.")
		actualTracerProvider = cfg.ExternalTracerProvider
		if *c.config.ManageGlobalProviders {
			otel.SetTracerProvider(cfg.ExternalTracerProvider)
			cfg.AppLogger.Info("xylium-otel: External trace.TracerProvider set as global OTel provider.")
		}
	} else if cfg.Exporter != ExporterNone {
		tp, err := c.initInternalTracerProvider() // initInternalTracerProvider now takes Connector receiver
		if err != nil {
			return nil, fmt.Errorf("xylium-otel: failed to initialize internal TracerProvider: %w", err)
		}
		c.tracerProvider = tp // Store the internally managed SDK TracerProvider
		actualTracerProvider = tp
		if *c.config.ManageGlobalProviders {
			otel.SetTracerProvider(tp)
			cfg.AppLogger.Infof("xylium-otel: Internal TracerProvider (Exporter: %s) initialized and set as global OTel provider.", cfg.Exporter)
		} else {
			cfg.AppLogger.Infof("xylium-otel: Internal TracerProvider (Exporter: %s) initialized but NOT set as global (ManageGlobalProviders is false).", cfg.Exporter)
		}
	} else {
		cfg.AppLogger.Info("xylium-otel: No external TracerProvider and Exporter is 'none'. Connector will be NoOp for tracing unless a global provider is set elsewhere.")
		c.isNoOp = true
		actualTracerProvider = otel.GetTracerProvider() // Fallback to global (which might be NoOp)
	}

	// Setup Propagator
	if cfg.Propagator != nil {
		c.propagator = cfg.Propagator
		if *c.config.ManageGlobalProviders {
			otel.SetTextMapPropagator(c.propagator)
			cfg.AppLogger.Info("xylium-otel: Custom Propagator configured and set as global OTel propagator.")
		} else {
			cfg.AppLogger.Info("xylium-otel: Custom Propagator configured but NOT set as global (ManageGlobalProviders is false).")
		}
	} else {
		c.propagator = propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, // W3C Trace Context
			propagation.Baggage{},      // W3C Baggage
		)
		if *c.config.ManageGlobalProviders {
			otel.SetTextMapPropagator(c.propagator)
			cfg.AppLogger.Info("xylium-otel: Default Propagator (TraceContext & Baggage) set as global OTel propagator.")
		} else {
			cfg.AppLogger.Info("xylium-otel: Default Propagator (TraceContext & Baggage) configured but NOT set as global (ManageGlobalProviders is false).")
		}
	}

	// Setup the tracer instance for the connector itself
	// Use a distinct name for the connector's own tracer (used by middleware).
	// If ManageGlobalProviders is false, this tracer comes from the internal TP,
	// otherwise from the (now potentially set) global TP.
	c.tracer = actualTracerProvider.Tracer("xylium-otel-connector", trace.WithInstrumentationVersion("xylium-otel/vNext")) // TODO: Add actual version

	if c.isNoOp {
		cfg.AppLogger.Warn("xylium-otel: Connector initialized in NoOp mode. Tracing middleware will be a pass-through.")
	}

	cfg.AppLogger.Info("xylium-otel: Connector initialization complete.")
	return c, nil
}

// initInternalTracerProvider initializes and returns an *sdktrace.TracerProvider
// based on the connector's internal configuration (Exporter, OTLP, Sampler, Resource).
// This method is called by New() if no external provider is given and Exporter is not "none".
func (c *Connector) initInternalTracerProvider() (*sdktrace.TracerProvider, error) {
	var exporter sdktrace.SpanExporter
	var err error

	c.config.AppLogger.Debugf("xylium-otel: Initializing internal OTel exporter of type '%s'.", c.config.Exporter)

	switch c.config.Exporter {
	case ExporterOTLPGRPC:
		if c.config.OTLP.Endpoint == "" {
			return nil, errors.New("xylium-otel: OTLPConfig.Endpoint is required for OTLP gRPC exporter")
		}
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(c.config.OTLP.Endpoint)}
		if c.config.OTLP.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		if len(c.config.OTLP.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(c.config.OTLP.Headers))
		}
		if c.config.OTLP.Timeout > 0 {
			opts = append(opts, otlptracegrpc.WithTimeout(c.config.OTLP.Timeout))
		}

		// Create context for exporter creation, can be short-lived.
		exporterCtx, cancel := context.WithTimeout(context.Background(), c.config.OTLP.Timeout) // Use configured timeout or a default
		defer cancel()

		exporter, err = otlptracegrpc.New(exporterCtx, opts...)
		if err != nil {
			return nil, fmt.Errorf("xylium-otel: creating OTLP gRPC exporter to '%s': %w", c.config.OTLP.Endpoint, err)
		}
		c.config.AppLogger.Infof("xylium-otel: OTLP gRPC exporter configured for endpoint: %s (Insecure: %t, Timeout: %v).", c.config.OTLP.Endpoint, c.config.OTLP.Insecure, c.config.OTLP.Timeout)

	case ExporterStdout:
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("xylium-otel: creating stdout trace exporter: %w", err)
		}
		c.config.AppLogger.Info("xylium-otel: Stdout trace exporter configured (pretty print enabled).")

	default: // Should not happen if New() validates ExporterType for internal setup.
		return nil, fmt.Errorf("xylium-otel: unsupported exporter type '%s' for internal TracerProvider setup", c.config.Exporter)
	}

	// Create OTel Resource
	resAttrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(c.config.ServiceName),
	}
	if c.config.ServiceVersion != "" {
		resAttrs = append(resAttrs, semconv.ServiceVersionKey.String(c.config.ServiceVersion))
	}
	if c.config.Environment != "" {
		resAttrs = append(resAttrs, semconv.DeploymentEnvironmentKey.String(c.config.Environment))
	}

	// Merge with default resource (e.g., for host, OS attributes).
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, resAttrs...),
	)
	if err != nil {
		// Attempt to shutdown the exporter if resource creation fails to prevent leaks.
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second) // Short timeout for exporter shutdown
		defer cancelShutdown()
		if cerr := exporter.Shutdown(shutdownCtx); cerr != nil {
			c.config.AppLogger.Errorf("xylium-otel: Failed to shutdown exporter after resource creation error: %v (Original resource error: %v)", cerr, err)
		}
		return nil, fmt.Errorf("xylium-otel: merging OTel resources: %w", err)
	}

	// Create and return the SDK TracerProvider.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(c.config.Sampler), // Use configured sampler
	)
	return tp, nil
}

// GetTracer returns a trace.Tracer instance.
// If ManageGlobalProviders is false and an internal TracerProvider was initialized,
// it returns a tracer from that internal provider. Otherwise, it returns a tracer
// from the (potentially globally set) OTel TracerProvider.
// `instrumentationName` is the name of the library or component creating spans.
// `opts` are optional `trace.TracerOption`s.
func (c *Connector) GetTracer(instrumentationName string, opts ...trace.TracerOption) trace.Tracer {
	if c.isNoOp {
		// Return a NoOpTracer or tracer from global NoOpProvider
		return otel.GetTracerProvider().Tracer(instrumentationName, opts...)
	}

	if c.config.ManageGlobalProviders != nil && !*c.config.ManageGlobalProviders {
		// If not managing globals, and we have an internal SDK provider, use it.
		if c.tracerProvider != nil {
			return c.tracerProvider.Tracer(instrumentationName, opts...)
		}
		// If no internal provider (e.g., external provider was given but not SDK type, or init failed silently to NoOp),
		// then this tracer might effectively be NoOp if global isn't set elsewhere.
		// This branch implies `ExternalSDKTracerProvider` was nil, and `initInternalTracerProvider` didn't run or set `c.tracerProvider`.
		// It might also mean `ExternalTracerProvider` (non-SDK) was used.
		// In this case, for consistency, we can still try to use the `c.tracer` which was initialized using the
		// `actualTracerProvider` in `New()`.
		if c.tracer != nil {
			return c.tracer // Return the tracer initialized in New()
		}
		// Fallback: should not be commonly hit if New() logic is correct.
		c.config.AppLogger.Warnf("xylium-otel: GetTracer called when ManageGlobalProviders is false, but internal tracer/provider is not fully set. Falling back to global OTel provider for tracer '%s'.", instrumentationName)
		return otel.GetTracerProvider().Tracer(instrumentationName, opts...)
	}

	// Default: ManageGlobalProviders is true or nil (defaulting to true)
	// Use the global TracerProvider (which this connector might have set).
	return otel.Tracer(instrumentationName, opts...)
}

// Propagator returns the configured TextMapPropagator.
// If ManageGlobalProviders is false, it returns the propagator instance held by the connector.
// Otherwise, it returns the global OTel propagator.
func (c *Connector) Propagator() propagation.TextMapPropagator {
	if c.isNoOp {
		return propagation.NewCompositeTextMapPropagator() // Return a NoOp-safe default
	}

	if c.config.ManageGlobalProviders != nil && !*c.config.ManageGlobalProviders {
		// Return the connector's internally stored propagator instance.
		if c.propagator != nil {
			return c.propagator
		}
		// Fallback if c.propagator somehow nil (should not happen if New() is correct)
		c.config.AppLogger.Warn("xylium-otel: Propagator() called when ManageGlobalProviders is false, but internal propagator is nil. Falling back to global OTel propagator.")
		return otel.GetTextMapPropagator()
	}

	// Default: ManageGlobalProviders is true or nil
	// Use the global TextMapPropagator (which this connector might have set).
	return otel.GetTextMapPropagator()
}

// Close shuts down the internally managed TracerProvider, if one was created by this connector.
// It respects the Config.ShutdownTimeout. If an external TracerProvider was used,
// this method is a no-op for the provider's lifecycle.
// Implements io.Closer, allowing Xylium to manage its lifecycle during graceful shutdown
// when the connector instance is stored using `app.AppSet()`.
func (c *Connector) Close() error {
	if c.isNoOp {
		if c.config.AppLogger != nil { // Check logger existence before using
			c.config.AppLogger.Debug("xylium-otel: Close() called on a NoOp connector. Nothing to shut down.")
		}
		return nil
	}

	// Only shutdown the tracerProvider if it was internally created and managed by this connector.
	// c.tracerProvider (the *sdktrace.TracerProvider) is only non-nil if created internally.
	if c.tracerProvider != nil {
		if c.config.AppLogger != nil {
			c.config.AppLogger.Infof("xylium-otel: Shutting down internally managed OpenTelemetry TracerProvider (Timeout: %v)...", c.config.ShutdownTimeout)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), c.config.ShutdownTimeout)
		defer cancel()

		if err := c.tracerProvider.Shutdown(shutdownCtx); err != nil {
			if c.config.AppLogger != nil {
				c.config.AppLogger.Errorf("xylium-otel: Error shutting down managed TracerProvider: %v", err)
			}
			return fmt.Errorf("xylium-otel: shutting down managed TracerProvider: %w", err)
		}
		if c.config.AppLogger != nil {
			c.config.AppLogger.Info("xylium-otel: Internally managed TracerProvider shut down successfully.")
		}
		return nil
	}

	if c.config.AppLogger != nil {
		c.config.AppLogger.Info("xylium-otel: Close() called, but TracerProvider was externally managed or not initialized by this connector. No internal shutdown performed.")
	}
	return nil
}

// IsNoOp returns true if the connector is configured to be a no-operation instance
// (e.g., due to Config.Disabled being true or inability to initialize a TracerProvider).
// Middleware and other operations will effectively be pass-throughs if IsNoOp is true.
func (c *Connector) IsNoOp() bool {
	return c.isNoOp
}

// Ensure Connector implements io.Closer for Xylium's graceful shutdown.
var _ io.Closer = (*Connector)(nil)
