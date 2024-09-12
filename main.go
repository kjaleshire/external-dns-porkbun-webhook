package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	porkbun "github.com/kjaleshire/external-dns-porkbun-webhook/provider"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webhook "sigs.k8s.io/external-dns/provider/webhook/api"
)

var (
	logFormat         = kingpin.Flag("log-format", "The format in which log messages are printed (default: text, options: logfmt, json)").Default("logfmt").Envar("PORKBUN_LOG_FORMAT").String()
	logLevel          = kingpin.Flag("log-level", "Set the level of logging. (default: info, options: panic, debug, info, warning, error, fatal)").Default("info").Envar("PORKBUN_LOG_LEVEL").String()
	listenAddr        = kingpin.Flag("listen-address", "The address this plugin listens on").Default(":8888").Envar("PORKBUN_LISTEN_ADDRESS").String()
	metricsListenAddr = kingpin.Flag("metrics-listen-address", "The address this plugin provides metrics on").Default(":8889").Envar("PORKBUN_METRICS_LISTEN_ADDRESS").String()
	tlsConfig         = kingpin.Flag("tls-config", "Path to TLS config file.").Envar("PORKBUN_TLS_CONFIG").Default("").String()

	domainFilter = kingpin.Flag("domain-filter", "Limit possible target zones by a domain suffix; specify multiple times for multiple domains").Required().Envar("PORKBUN_DOMAIN_FILTER").Strings()
	dryRun       = kingpin.Flag("dry-run", "Run without connecting to Porkbun's API").Default("false").Envar("PORKBUN_DRY_RUN").Bool()
	apiKey       = kingpin.Flag("porkbun-api-key", "The API key to connect to Porkbun's API").Required().Envar("PORKBUN_API_KEY").String()
	apiSecret    = kingpin.Flag("porkbun-secret-key", "The API secret to connect to Porkbun's API").Required().Envar("PORKBUN_SECRET_KEY").String()
)

func main() {

	kingpin.Version(version.Info())
	kingpin.Parse()

	var logger log.Logger
	switch *logFormat {
	case "json":
		logger = log.NewJSONLogger(log.NewSyncWriter(os.Stderr))
	case "logfmt":
		logger = log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	default:
		fmt.Printf("Error: Unknown log format: %s\n", *logFormat)
		os.Exit(1)
	}
	logger = level.NewFilter(logger, level.Allow(level.ParseDefault(*logLevel, level.InfoValue())))
	logger = log.With(logger, "ts", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
	_ = level.Info(logger).Log("msg", "starting external-dns Porkbun webhook plugin", "version", version.Version, "revision", version.Revision)
	_ = level.Debug(logger).Log("api-key", *apiKey, "api-secret", *apiSecret)

	prometheus.DefaultRegisterer.MustRegister(version.NewCollector("external_dns_porkbun"))

	metricsMux := buildMetricsServer(prometheus.DefaultGatherer, logger)
	metricsServer := http.Server{
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second}

	metricsFlags := web.FlagConfig{
		WebListenAddresses: &[]string{*metricsListenAddr},
		WebSystemdSocket:   new(bool),
		WebConfigFile:      tlsConfig,
	}

	webhookMux, err := buildWebhookServer(logger)
	if err != nil {
		_ = level.Error(logger).Log("msg", "Failed to create provider", "error", err)
		os.Exit(1)
	}
	webhookServer := http.Server{
		Handler:           webhookMux,
		ReadHeaderTimeout: 5 * time.Second}

	webhookFlags := web.FlagConfig{
		WebListenAddresses: &[]string{*listenAddr},
		WebSystemdSocket:   new(bool),
		WebConfigFile:      tlsConfig,
	}

	var g run.Group

	// Run Metrics server
	{
		g.Add(func() error {
			_ = level.Info(logger).Log("msg", "Started external-dns-porkbun-webhook metrics server", "address", metricsListenAddr)
			return web.ListenAndServe(&metricsServer, &metricsFlags, logger)
		}, func(error) {
			ctxShutDown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = metricsServer.Shutdown(ctxShutDown)
		})
	}
	// Run webhook API server
	{
		g.Add(func() error {
			_ = level.Info(logger).Log("msg", "Started external-dns-porkbun-webhook webhook server", "address", listenAddr)
			return web.ListenAndServe(&webhookServer, &webhookFlags, logger)
		}, func(error) {
			ctxShutDown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = webhookServer.Shutdown(ctxShutDown)
		})
	}

	if err := g.Run(); err != nil {
		_ = level.Error(logger).Log("msg", "run server group error", "error", err)
		os.Exit(1)
	}

}

func buildMetricsServer(registry prometheus.Gatherer, logger log.Logger) *http.ServeMux {
	mux := http.NewServeMux()

	var healthzPath = "/healthz"
	var metricsPath = "/metrics"
	var rootPath = "/"

	// Add metricsPath
	mux.Handle(metricsPath, promhttp.HandlerFor(
		registry,
		promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		}))

	// Add healthzPath
	mux.HandleFunc(healthzPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(http.StatusText(http.StatusOK)))
	})

	// Add index
	landingConfig := web.LandingConfig{
		Name:        "external-dns-porkbun-webhook",
		Description: "external-dns webhook provider for Porkbun",
		Version:     version.Info(),
		Links: []web.LandingLinks{
			{
				Address: metricsPath,
				Text:    "Metrics",
			},
		},
	}
	landingPage, err := web.NewLandingPage(landingConfig)
	if err != nil {
		_ = level.Error(logger).Log("msg", "failed to create landing page", "error", err)
	}
	mux.Handle(rootPath, landingPage)

	return mux
}

func buildWebhookServer(logger log.Logger) (*http.ServeMux, error) {
	mux := http.NewServeMux()

	var rootPath = "/"
	var recordsPath = "/records"
	var adjustEndpointsPath = "/adjustendpoints"

	ncProvider, err := porkbun.NewPorkbunProvider(domainFilter, *apiKey, *apiSecret, *dryRun, logger)
	if err != nil {
		return nil, err
	}

	p := webhook.WebhookServer{
		Provider: ncProvider,
	}

	// Add negotiatePath
	mux.HandleFunc(rootPath, p.NegotiateHandler)
	// Add adjustEndpointsPath
	mux.HandleFunc(adjustEndpointsPath, p.AdjustEndpointsHandler)
	// Add recordsPath
	mux.HandleFunc(recordsPath, p.RecordsHandler)

	return mux, nil
}
