/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"github.com/knative/eventing/pkg/tracing"
	"log"
	"time"

	informers "github.com/knative/eventing/pkg/client/informers/externalversions"
	dispatcher "github.com/knative/eventing/pkg/inmemorychannel"
	"github.com/knative/eventing/pkg/logconfig"
	"github.com/knative/eventing/pkg/provisioners/swappable"
	"github.com/knative/eventing/pkg/reconciler"
	inmemorychannel "github.com/knative/eventing/pkg/reconciler/inmemorychannel/dispatcher"
	"github.com/knative/pkg/configmap"
	kncontroller "github.com/knative/pkg/controller"
	"github.com/knative/pkg/logging"
	"github.com/knative/pkg/signals"
	"go.uber.org/zap"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	hardcodedLoggingConfig = flag.Bool("hardCodedLoggingConfig", false, "If true, use the hard coded logging config. It is intended to be used only when debugging outside a Kubernetes cluster.")
	masterURL              = flag.String("master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	kubeconfig             = flag.String("kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")

	readTimeout  = 1 * time.Minute
	writeTimeout = 1 * time.Minute
	port         = 8080
)

func main() {
	flag.Parse()
	logger, atomicLevel := setupLogger()
	defer logger.Sync()

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()

	cfg, err := clientcmd.BuildConfigFromFlags(*masterURL, *kubeconfig)
	if err != nil {
		logger.Fatalw("Error building kubeconfig", zap.Error(err))
	}

	sh, err := swappable.NewEmptyHandler(logger.Desugar())
	if err != nil {
		logger.Fatal("Error creating swappable.Handler", zap.Error(err))
	}

	args := &dispatcher.InMemoryDispatcherArgs{
		Port:         port,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		Handler:      sh,
		Logger:       logger.Desugar(),
	}
	inMemoryDispatcher := dispatcher.NewDispatcher(args)

	logger = logger.With(zap.String("controller/impl", "pkg"))
	logger.Info("Starting the InMemory dispatcher")

	const numControllers = 1
	cfg.QPS = numControllers * rest.DefaultQPS
	cfg.Burst = numControllers * rest.DefaultBurst
	opt := reconciler.NewOptionsOrDie(cfg, logger, stopCh)

	eventingInformerFactory := informers.NewSharedInformerFactory(opt.EventingClientSet, opt.ResyncPeriod)

	// Messaging
	inMemoryChannelInformer := eventingInformerFactory.Messaging().V1alpha1().InMemoryChannels()

	// Build all of our controllers, with the clients constructed above.
	// Add new controllers to this array.
	// You also need to modify numControllers above to match this.
	controllers := [...]*kncontroller.Impl{
		inmemorychannel.NewController(
			opt,
			inMemoryDispatcher,
			inMemoryChannelInformer,
		),
	}
	// This line asserts at compile time that the length of controllers is equal to numControllers.
	// It is based on https://go101.org/article/tips.html#assert-at-compile-time, which notes that
	// var _ [N-M]int
	// asserts at compile time that N >= M, which we can use to establish equality of N and M:
	// (N >= M) && (M >= N) => (N == M)
	var _ [numControllers - len(controllers)][len(controllers) - numControllers]int

	// Watch the logging config map and dynamically update logging levels.
	opt.ConfigMapWatcher.Watch(logconfig.ConfigMapName(), logging.UpdateLevelFromConfigMap(logger, atomicLevel, logconfig.Controller))
	// TODO: Watch the observability config map and dynamically update metrics exporter.
	//opt.ConfigMapWatcher.Watch(metrics.ObservabilityConfigName, metrics.UpdateExporterFromConfigMap(component, logger))

	// Setup zipkin tracing.
	if err = tracing.SetupDynamicZipkinPublishing(logger, opt.ConfigMapWatcher, "imc-dispatcher"); err != nil {
		logger.Fatalw("Error setting up Zipkin publishing", zap.Error(err))
	}

	if err := opt.ConfigMapWatcher.Start(stopCh); err != nil {
		logger.Fatalw("failed to start configuration manager", zap.Error(err))
	}

	// Start all of the informers and wait for them to sync.
	logger.Info("Starting informers.")
	if err := kncontroller.StartInformers(
		stopCh,
		// Messaging
		inMemoryChannelInformer.Informer(),
	); err != nil {
		logger.Fatalf("Failed to start informers: %v", err)
	}

	go inMemoryDispatcher.Start(stopCh)

	logger.Info("Starting controllers.")
	kncontroller.StartAll(stopCh, controllers[:]...)

	inMemoryDispatcher.Stop()
}

func setupLogger() (*zap.SugaredLogger, zap.AtomicLevel) {
	// Set up our logger.
	loggingConfigMap := getLoggingConfigOrDie()
	loggingConfig, err := logging.NewConfigFromMap(loggingConfigMap)
	if err != nil {
		log.Fatalf("Error parsing logging configuration: %v", err)
	}
	return logging.NewLoggerFromConfig(loggingConfig, logconfig.Controller)
}

func getLoggingConfigOrDie() map[string]string {
	if hardcodedLoggingConfig != nil && *hardcodedLoggingConfig {
		return map[string]string{
			"loglevel.controller": "info",
			"zap-logger-config": `
				{
					"level": "info",
					"development": false,
					"outputPaths": ["stdout"],
					"errorOutputPaths": ["stderr"],
					"encoding": "json",
					"encoderConfig": {
					"timeKey": "ts",
					"levelKey": "level",
					"nameKey": "logger",
					"callerKey": "caller",
					"messageKey": "msg",
					"stacktraceKey": "stacktrace",
					"lineEnding": "",
					"levelEncoder": "",
					"timeEncoder": "iso8601",
					"durationEncoder": "",
					"callerEncoder": ""
				}`,
		}
	} else {
		cm, err := configmap.Load("/etc/config-logging")
		if err != nil {
			log.Fatalf("Error loading logging configuration: %v", err)
		}
		return cm
	}
}