// Package xyliumotel provides the OpenTelemetry connector for the Xylium framework.
package xyliumotel

import (
	"context"
	"errors"
	"fmt"
	"io" // Untuk io.Closer
	"time"

	"github.com/arwahdevops/xylium-core/src/xylium"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// ExporterType defines the type of OTel exporter to configure.
type ExporterType string

const (
	ExporterOTLPGRPC ExporterType = "otlp_grpc"
	ExporterStdout   ExporterType = "stdout"
	ExporterNone     ExporterType = "none"
)

// OTLPConfig holds configuration specific to the OTLP exporter.
type OTLPConfig struct {
	Endpoint string
	Insecure bool
}

// Config holds all configuration options for initializing the OpenTelemetry Connector.
type Config struct {
	AppLogger                 xylium.Logger
	ServiceName               string
	ServiceVersion            string
	Environment               string
	Exporter                  ExporterType
	OTLP                      OTLPConfig
	ExternalTracerProvider    trace.TracerProvider
	ExternalSDKTracerProvider *sdktrace.TracerProvider
	Propagator                propagation.TextMapPropagator
	Sampler                   sdktrace.Sampler
	ShutdownTimeout           time.Duration
	Disabled                  bool
}

// Connector is the Xylium-aware wrapper for OpenTelemetry.
type Connector struct {
	config         Config
	tracerProvider *sdktrace.TracerProvider
	propagator     propagation.TextMapPropagator
	isNoOp         bool
}

// New creates and initializes a new OpenTelemetry Connector.
func New(cfg Config) (*Connector, error) {
	if cfg.Disabled {
		if cfg.AppLogger != nil {
			cfg.AppLogger.Info("xylium-otel: OpenTelemetry is disabled by configuration.")
		}
		return &Connector{isNoOp: true, config: cfg}, nil
	}

	if cfg.AppLogger == nil {
		return nil, errors.New("xylium-otel: Config.AppLogger is required")
	}
	if cfg.ServiceName == "" && cfg.ExternalTracerProvider == nil && cfg.ExternalSDKTracerProvider == nil {
		return nil, errors.New("xylium-otel: Config.ServiceName is required when not providing an external TracerProvider or SDKTracerProvider")
	}

	if cfg.Exporter == "" {
		currentMode := xylium.Mode()
		if currentMode == xylium.ReleaseMode {
			cfg.Exporter = ExporterNone
		} else {
			cfg.Exporter = ExporterStdout
		}
		cfg.AppLogger.Infof("xylium-otel: Exporter type not specified, defaulted to '%s' based on Xylium mode '%s'.", cfg.Exporter, currentMode)
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 5 * time.Second
	}
	if cfg.Sampler == nil {
		cfg.Sampler = sdktrace.ParentBased(sdktrace.AlwaysSample())
	}

	c := &Connector{config: cfg}

	if cfg.ExternalSDKTracerProvider != nil {
		cfg.AppLogger.Info("xylium-otel: Using pre-configured external SDK TracerProvider (*sdktrace.TracerProvider). Connector will not manage its lifecycle or global registration.")
		c.tracerProvider = nil
		otel.SetTracerProvider(cfg.ExternalSDKTracerProvider)
	} else if cfg.ExternalTracerProvider != nil {
		cfg.AppLogger.Info("xylium-otel: Using pre-configured external TracerProvider (trace.TracerProvider). Connector will not manage its lifecycle or global registration.")
		c.tracerProvider = nil
		otel.SetTracerProvider(cfg.ExternalTracerProvider)
	} else if cfg.Exporter != ExporterNone {
		tp, err := c.initInternalTracerProvider()
		if err != nil {
			return nil, fmt.Errorf("xylium-otel: failed to initialize internal TracerProvider: %w", err)
		}
		c.tracerProvider = tp
		otel.SetTracerProvider(tp)
		cfg.AppLogger.Infof("xylium-otel: Internal TracerProvider initialized and set globally (Exporter: %s).", cfg.Exporter)
	} else {
		cfg.AppLogger.Info("xylium-otel: No specific TracerProvider configured and Exporter is 'none'. Global OTel TracerProvider remains unchanged. Ensure SDK is configured externally if tracing is desired.")
		c.isNoOp = true
	}

	if cfg.Propagator != nil {
		c.propagator = cfg.Propagator
		otel.SetTextMapPropagator(c.propagator)
		cfg.AppLogger.Info("xylium-otel: Using custom Propagator and set globally.")
	} else {
		c.propagator = propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		)
		otel.SetTextMapPropagator(c.propagator)
		cfg.AppLogger.Info("xylium-otel: Default Propagator (TraceContext & Baggage) set globally.")
	}

	if c.isNoOp && cfg.Exporter != ExporterNone && cfg.ExternalTracerProvider == nil && cfg.ExternalSDKTracerProvider == nil {
		cfg.AppLogger.Warn("xylium-otel: Connector is in no-op mode because no TracerProvider could be set up (Exporter=none and no external provider). Middleware will be a pass-through.")
	}

	cfg.AppLogger.Info("xylium-otel: Connector successfully initialized.")
	return c, nil
}

func (c *Connector) initInternalTracerProvider() (*sdktrace.TracerProvider, error) {
	var exporter sdktrace.SpanExporter
	var err error

	c.config.AppLogger.Debugf("xylium-otel: Initializing internal exporter of type '%s'.", c.config.Exporter)

	switch c.config.Exporter {
	case ExporterOTLPGRPC:
		if c.config.OTLP.Endpoint == "" {
			return nil, errors.New("xylium-otel: OTLPConfig.Endpoint is required for OTLP gRPC exporter")
		}
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(c.config.OTLP.Endpoint)}
		if c.config.OTLP.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exporter, err = otlptracegrpc.New(context.Background(), opts...)
		if err != nil {
			return nil, fmt.Errorf("creating OTLP gRPC exporter: %w", err)
		}
		c.config.AppLogger.Infof("xylium-otel: OTLP gRPC exporter configured for endpoint: %s (Insecure: %t).", c.config.OTLP.Endpoint, c.config.OTLP.Insecure)

	case ExporterStdout:
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("creating stdout exporter: %w", err)
		}
		c.config.AppLogger.Info("xylium-otel: Stdout exporter configured (pretty print enabled).")

	default:
		return nil, fmt.Errorf("xylium-otel: unsupported exporter type for internal setup: %s", c.config.Exporter)
	}

	resAttrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(c.config.ServiceName),
	}
	if c.config.ServiceVersion != "" {
		resAttrs = append(resAttrs, semconv.ServiceVersionKey.String(c.config.ServiceVersion))
	}
	if c.config.Environment != "" {
		resAttrs = append(resAttrs, semconv.DeploymentEnvironmentKey.String(c.config.Environment))
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, resAttrs...),
	)
	if err != nil {
		if cerr := exporter.Shutdown(context.Background()); cerr != nil {
			c.config.AppLogger.Errorf("xylium-otel: Failed to shutdown exporter after resource creation error: %v", cerr)
		}
		return nil, fmt.Errorf("creating resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(c.config.Sampler),
	)
	return tp, nil
}

func (c *Connector) GetTracer(instrumentationName string, opts ...trace.TracerOption) trace.Tracer {
	if c.isNoOp {
		return otel.GetTracerProvider().Tracer(instrumentationName, opts...)
	}
	return otel.Tracer(instrumentationName, opts...)
}

func (c *Connector) Propagator() propagation.TextMapPropagator {
	if c.isNoOp || c.propagator == nil {
		return propagation.NewCompositeTextMapPropagator()
	}
	return c.propagator
}

func (c *Connector) Close() error {
	if c.isNoOp {
		if c.config.AppLogger != nil {
			c.config.AppLogger.Debug("xylium-otel: Close called on no-op connector.")
		}
		return nil
	}
	if c.tracerProvider != nil {
		if c.config.AppLogger != nil {
			c.config.AppLogger.Infof("xylium-otel: Shutting down managed TracerProvider (timeout: %v)...", c.config.ShutdownTimeout)
		}
		ctx, cancel := context.WithTimeout(context.Background(), c.config.ShutdownTimeout)
		defer cancel()
		if err := c.tracerProvider.Shutdown(ctx); err != nil {
			if c.config.AppLogger != nil {
				c.config.AppLogger.Errorf("xylium-otel: Error shutting down TracerProvider: %v", err)
			}
			return fmt.Errorf("xylium-otel: tracer provider shutdown: %w", err)
		}
		if c.config.AppLogger != nil {
			c.config.AppLogger.Info("xylium-otel: Managed TracerProvider shut down successfully.")
		}
		return nil
	}
	if c.config.AppLogger != nil {
		c.config.AppLogger.Info("xylium-otel: Close called, but TracerProvider was externally managed or not initialized by this connector.")
	}
	return nil
}

func (c *Connector) IsNoOp() bool {
	return c.isNoOp
}

var _ io.Closer = (*Connector)(nil)
