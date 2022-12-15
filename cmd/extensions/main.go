// Copyright 2022 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Controller for gameservers
package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agones.dev/agones/pkg"
	"agones.dev/agones/pkg/client/clientset/versioned"
	"agones.dev/agones/pkg/client/informers/externalversions"
	"agones.dev/agones/pkg/cloudproduct"
	"agones.dev/agones/pkg/fleetautoscalers"
	"agones.dev/agones/pkg/fleets"
	"agones.dev/agones/pkg/gameservers"
	"agones.dev/agones/pkg/gameserversets"
	"agones.dev/agones/pkg/metrics"
	"agones.dev/agones/pkg/util/https"
	"agones.dev/agones/pkg/util/runtime"
	"agones.dev/agones/pkg/util/signals"
	"agones.dev/agones/pkg/util/webhooks"
	"github.com/heptiolabs/healthcheck"
	"github.com/pkg/errors"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"gopkg.in/natefinch/lumberjack.v2"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	enableStackdriverMetricsFlag = "stackdriver-exporter"
	stackdriverLabels            = "stackdriver-labels"
	enablePrometheusMetricsFlag  = "prometheus-exporter"
	projectIDFlag                = "gcp-project-id"
	certFileFlag                 = "cert-file"
	keyFileFlag                  = "key-file"
	numWorkersFlag               = "num-workers"
	logDirFlag                   = "log-dir"
	logLevelFlag                 = "log-level"
	logSizeLimitMBFlag           = "log-size-limit-mb"
	kubeconfigFlag               = "kubeconfig"
	defaultResync                = 30 * time.Second
)

var (
	logger = runtime.NewLoggerWithSource("main")
)

func setupLogging(logDir string, logSizeLimitMB int) {
	logFileName := filepath.Join(logDir, "agones-extensions-"+time.Now().Format("20060102_150405")+".log")

	const maxLogSizeMB = 100
	maxBackups := (logSizeLimitMB - maxLogSizeMB) / maxLogSizeMB
	logger.WithField("filename", logFileName).WithField("numbackups", maxBackups).Info("logging to file")
	logrus.SetOutput(
		io.MultiWriter(
			logrus.StandardLogger().Out,
			&lumberjack.Logger{
				Filename:   logFileName,
				MaxSize:    maxLogSizeMB,
				MaxBackups: maxBackups,
			},
		),
	)
}

// main starts the operator for the gameserver CRD
func main() {
	ctx := signals.NewSigKillContext()
	ctlConf := parseEnvFlags()

	if ctlConf.LogDir != "" {
		setupLogging(ctlConf.LogDir, ctlConf.LogSizeLimitMB)
	}

	logger.WithField("logLevel", ctlConf.LogLevel).Info("Setting LogLevel configuration")
	level, err := logrus.ParseLevel(strings.ToLower(ctlConf.LogLevel))
	if err == nil {
		runtime.SetLevel(level)
	} else {
		logger.WithError(err).Info("Specified wrong Logging.SdkServer. Setting default loglevel - Info")
		runtime.SetLevel(logrus.InfoLevel)
	}

	logger.WithField("version", pkg.Version).WithField("featureGates", runtime.EncodeFeatures()).
		WithField("ctlConf", ctlConf).Info("starting gameServer operator...")

	if errs := ctlConf.validate(); len(errs) != 0 {
		for _, err := range errs {
			logger.Error(err)
		}
		logger.Fatal("Could not create extensions from environment or flags")
	}

	// if the kubeconfig fails BuildConfigFromFlags will try in cluster config
	clientConf, err := clientcmd.BuildConfigFromFlags("", ctlConf.KubeConfig)
	if err != nil {
		logger.WithError(err).Fatal("Could not create in cluster config")
	}

	kubeClient, err := kubernetes.NewForConfig(clientConf)
	if err != nil {
		logger.WithError(err).Fatal("Could not create the kubernetes clientset")
	}

	agonesClient, err := versioned.NewForConfig(clientConf)
	if err != nil {
		logger.WithError(err).Fatal("Could not create the agones api clientset")
	}

	err = cloudproduct.Initialize(ctx, kubeClient)
	if err != nil {
		logger.WithError(err).Fatal("Could not initialize cloud product")
	}
	// https server and the items that share the Mux for routing
	httpsServer := https.NewServer(ctlConf.CertFile, ctlConf.KeyFile)
	wh := webhooks.NewWebHook(httpsServer.Mux)

	agonesInformerFactory := externalversions.NewSharedInformerFactory(agonesClient, defaultResync)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, defaultResync)

	server := &httpServer{}
	var rs []runner
	var health healthcheck.Handler

	// Stackdriver metrics
	if ctlConf.Stackdriver {
		sd, err := metrics.RegisterStackdriverExporter(ctlConf.GCPProjectID, ctlConf.StackdriverLabels)
		if err != nil {
			logger.WithError(err).Fatal("Could not register stackdriver exporter")
		}
		// It is imperative to invoke flush before your main function exits
		defer sd.Flush()
	}

	// Prometheus metrics
	if ctlConf.PrometheusMetrics {
		registry := prom.NewRegistry()
		metricHandler, err := metrics.RegisterPrometheusExporter(registry)
		if err != nil {
			logger.WithError(err).Fatal("Could not register prometheus exporter")
		}
		server.Handle("/metrics", metricHandler)
		health = healthcheck.NewMetricsHandler(registry, "agones")
	} else {
		health = healthcheck.NewHandler()
	}

	// If we are using Prometheus only exporter we can make reporting more often,
	// every 1 seconds, if we are using Stackdriver we would use 60 seconds reporting period,
	// which is a requirements of Stackdriver, otherwise most of time series would be invalid for Stackdriver
	metrics.SetReportingPeriod(ctlConf.PrometheusMetrics, ctlConf.Stackdriver)

	server.Handle("/", health)

	gameservers.NewExtensions(wh)
	gameserversets.NewExtensions(wh)
	fleets.NewExtensions(wh)
	fleetautoscalers.NewExtensions(wh)

	rs = append(rs, httpsServer, server)

	kubeInformerFactory.Start(ctx.Done())
	agonesInformerFactory.Start(ctx.Done())

	for _, r := range rs {
		go func(rr runner) {
			if runErr := rr.Run(ctx, ctlConf.NumWorkers); runErr != nil {
				logger.WithError(runErr).Fatalf("could not start runner: %T", rr)
			}
		}(r)
	}

	<-ctx.Done()
	logger.Info("Shut down agones extensions")
}

func parseEnvFlags() config {
	exec, err := os.Executable()
	if err != nil {
		logger.WithError(err).Fatal("Could not get executable path")
	}

	base := filepath.Dir(exec)
	viper.SetDefault(certFileFlag, filepath.Join(base, "certs", "server.crt"))
	viper.SetDefault(keyFileFlag, filepath.Join(base, "certs", "server.key"))
	viper.SetDefault(enablePrometheusMetricsFlag, true)
	viper.SetDefault(enableStackdriverMetricsFlag, false)
	viper.SetDefault(stackdriverLabels, "")

	viper.SetDefault(projectIDFlag, "")
	viper.SetDefault(numWorkersFlag, 64)
	viper.SetDefault(logDirFlag, "")
	viper.SetDefault(logLevelFlag, "Info")
	viper.SetDefault(logSizeLimitMBFlag, 10000) // 10 GB, will be split into 100 MB chunks

	pflag.String(keyFileFlag, viper.GetString(keyFileFlag), "Optional. Path to the key file")
	pflag.String(certFileFlag, viper.GetString(certFileFlag), "Optional. Path to the crt file")
	pflag.String(kubeconfigFlag, viper.GetString(kubeconfigFlag), "Optional. kubeconfig to run the controller out of the cluster. Only use it for debugging as webhook won't works.")
	pflag.Bool(enablePrometheusMetricsFlag, viper.GetBool(enablePrometheusMetricsFlag), "Flag to activate metrics of Agones. Can also use PROMETHEUS_EXPORTER env variable.")
	pflag.Bool(enableStackdriverMetricsFlag, viper.GetBool(enableStackdriverMetricsFlag), "Flag to activate stackdriver monitoring metrics for Agones. Can also use STACKDRIVER_EXPORTER env variable.")
	pflag.String(stackdriverLabels, viper.GetString(stackdriverLabels), "A set of default labels to add to all stackdriver metrics generated. By default metadata are automatically added using Kubernetes API and GCP metadata enpoint.")
	pflag.String(projectIDFlag, viper.GetString(projectIDFlag), "GCP ProjectID used for Stackdriver, if not specified ProjectID from Application Default Credentials would be used. Can also use GCP_PROJECT_ID env variable.")
	pflag.Int32(numWorkersFlag, 64, "Number of controller workers per resource type")
	pflag.String(logDirFlag, viper.GetString(logDirFlag), "If set, store logs in a given directory.")
	pflag.Int32(logSizeLimitMBFlag, 1000, "Log file size limit in MB")
	pflag.String(logLevelFlag, viper.GetString(logLevelFlag), "Agones Log level")
	cloudproduct.BindFlags()
	runtime.FeaturesBindFlags()
	pflag.Parse()

	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	runtime.Must(viper.BindEnv(keyFileFlag))
	runtime.Must(viper.BindEnv(certFileFlag))
	runtime.Must(viper.BindEnv(kubeconfigFlag))
	runtime.Must(viper.BindEnv(enablePrometheusMetricsFlag))
	runtime.Must(viper.BindEnv(enableStackdriverMetricsFlag))
	runtime.Must(viper.BindEnv(stackdriverLabels))
	runtime.Must(viper.BindEnv(projectIDFlag))
	runtime.Must(viper.BindEnv(numWorkersFlag))
	runtime.Must(viper.BindEnv(logLevelFlag))
	runtime.Must(viper.BindEnv(logDirFlag))
	runtime.Must(viper.BindEnv(logSizeLimitMBFlag))
	runtime.Must(viper.BindPFlags(pflag.CommandLine))
	runtime.Must(cloudproduct.BindEnv())
	runtime.Must(runtime.FeaturesBindEnv())
	runtime.Must(runtime.ParseFeaturesFromEnv())

	return config{
		KeyFile:           viper.GetString(keyFileFlag),
		CertFile:          viper.GetString(certFileFlag),
		KubeConfig:        viper.GetString(kubeconfigFlag),
		PrometheusMetrics: viper.GetBool(enablePrometheusMetricsFlag),
		Stackdriver:       viper.GetBool(enableStackdriverMetricsFlag),
		GCPProjectID:      viper.GetString(projectIDFlag),
		NumWorkers:        int(viper.GetInt32(numWorkersFlag)),
		LogDir:            viper.GetString(logDirFlag),
		LogLevel:          viper.GetString(logLevelFlag),
		LogSizeLimitMB:    int(viper.GetInt32(logSizeLimitMBFlag)),
		StackdriverLabels: viper.GetString(stackdriverLabels),
	}
}

// config stores all required configuration to create a game server controller.
type config struct {
	PrometheusMetrics bool
	Stackdriver       bool
	StackdriverLabels string
	KeyFile           string
	CertFile          string
	KubeConfig        string
	GCPProjectID      string
	NumWorkers        int
	LogDir            string
	LogLevel          string
	LogSizeLimitMB    int
}

// validate ensures the ctlConfig data is valid.
func (c *config) validate() []error {
	validationErrors := make([]error, 0)
	return validationErrors
}

type runner interface {
	Run(ctx context.Context, workers int) error
}

type httpServer struct {
	http.ServeMux
}

func (h *httpServer) Run(_ context.Context, _ int) error {
	logger.Info("Starting http server...")
	srv := &http.Server{
		Addr:    ":8080",
		Handler: h,
	}
	defer srv.Close() // nolint: errcheck

	if err := srv.ListenAndServe(); err != nil {
		if err == http.ErrServerClosed {
			logger.WithError(err).Info("http server closed")
		} else {
			wrappedErr := errors.Wrap(err, "Could not listen on :8080")
			runtime.HandleError(logger.WithError(wrappedErr), wrappedErr)
		}
	}
	return nil
}
