// file: example/main.go
package main

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/arwahdevops/xylium-core/src/xylium" // Core Xylium
	xyliumotel "github.com/arwahdevops/xylium-otel" // Sesuaikan path ini jika berbeda

	"go.opentelemetry.io/otel/attribute"
)

func main() {
	// Inisialisasi Xylium App
	app := xylium.New()
	appLogger := app.Logger()

	// --- Konfigurasi dan Inisialisasi Xylium OTel Connector ---
	otelExporterEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	serviceName := "my-xylium-app-with-otel"
	serviceVersion := "1.0.1"
	environment := "development"

	exporterType := xyliumotel.ExporterStdout
	if otelExporterEndpoint != "" {
		exporterType = xyliumotel.ExporterOTLPGRPC
		appLogger.Infof("OTLP Exporter will be used with endpoint: %s", otelExporterEndpoint)
	} else {
		appLogger.Info("OTLP endpoint not set (OTEL_EXPORTER_OTLP_ENDPOINT). Defaulting to Stdout exporter for OpenTelemetry.")
	}

	otelConfig := xyliumotel.Config{
		AppLogger:      appLogger,
		ServiceName:    serviceName,
		ServiceVersion: serviceVersion,
		Environment:    environment,
		Exporter:       exporterType,
		OTLP: xyliumotel.OTLPConfig{
			Endpoint: otelExporterEndpoint,
			Insecure: true,
		},
	}

	otelConnector, err := xyliumotel.New(otelConfig)
	if err != nil {
		appLogger.Fatalf("Failed to initialize Xylium OTel connector: %v", err)
	}
	app.AppSet("otelConnector", otelConnector)

	// --- Middleware ---
	app.Use(xylium.RequestID())
	app.Use(otelConnector.OtelMiddleware())
	app.Use(xylium.Gzip())

	// --- Rute Aplikasi ---
	app.GET("/", func(c *xylium.Context) error {
		// Dapatkan tracer dari konektor
		tracer := otelConnector.GetTracer("handler.root") // Menggunakan method GetTracer dari Connector

		handlerCtx, span := tracer.Start(c.GoContext(), "rootEndpointHandlerLogic")
		defer span.End()

		span.SetAttributes(
			attribute.String("user.agent", c.UserAgent()),
			attribute.String("custom.data", "example_value"),
		)
		time.Sleep(15 * time.Millisecond)

		_, childSpan := tracer.Start(handlerCtx, "processInternalData")
		time.Sleep(5 * time.Millisecond)
		childSpan.SetAttributes(attribute.Bool("child.processed", true))
		childSpan.End()

		c.Logger().Infof("Root handler executed successfully for path: %s", c.Path())
		return c.JSON(http.StatusOK, xylium.M{
			"message":          "Hello from Xylium with integrated OpenTelemetry!",
			"serviceName":      serviceName,
			"traceIdFromCtx":   c.MustGet(xylium.ContextKeyOtelTraceID),
			"current_otel_env": os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		})
	})

	app.GET("/fast", func(c *xylium.Context) error {
		c.Logger().Info("Fast handler executed.")
		return c.String(http.StatusOK, "This was fast!")
	})

	app.GET("/error-test", func(c *xylium.Context) error {
		tracer := otelConnector.GetTracer("handler.error-test")
		_, span := tracer.Start(c.GoContext(), "errorGeneratingOperation")
		defer span.End()

		simulatedError := errors.New("simulated internal error in handler")
		span.RecordError(simulatedError)
		c.Logger().Errorf("Simulating an error: %v", simulatedError)
		return xylium.NewHTTPError(http.StatusInternalServerError, "An intentional error occurred.").WithInternal(simulatedError)
	})

	// --- Start Server ---
	listenAddr := ":8080"
	appLogger.Infof("Xylium server with OTel connector starting on http://localhost%s", listenAddr)
	appLogger.Infof("Service Name for OTel: %s", serviceName)
	if otelExporterEndpoint != "" {
		appLogger.Infof("OTel Exporter Endpoint: %s", otelExporterEndpoint)
	} else {
		appLogger.Infof("OTel Exporter: Stdout (no OTLP endpoint specified)")
	}

	if err := app.Start(listenAddr); err != nil {
		appLogger.Fatalf("Error starting Xylium server: %v", err)
	}
	appLogger.Info("Xylium server has shut down gracefully.")
}
