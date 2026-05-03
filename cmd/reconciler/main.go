package main

import (
	"flag"
	"os"
	"os/signal"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-co-op/gocron/v2"

	"github.com/k8snetworkplumbingwg/whereabouts/pkg/logging"
	"github.com/k8snetworkplumbingwg/whereabouts/pkg/reconciler"
)

const (
	reconcilerCronConfiguration = "/cron-schedule/config"
)

const (
	_ int = iota
	cronSchedulerCreationError
	fileWatcherError
	couldNotCreateConfigWatcherError
)

const defaultLogLevel = "debug"

func main() {
	logLevel := flag.String("log-level", defaultLogLevel, "Specify the reconciler application logging level")
	flag.Parse()
	if logLevel != nil && logging.GetLoggingLevel().String() != *logLevel {
		logging.SetLogLevel(*logLevel)
	}
	logging.SetLogStderr(true)

	stopChan := make(chan struct{})
	errorChan := make(chan error)
	defer close(stopChan)
	defer close(errorChan)
	handleSignals(stopChan, os.Interrupt)

	s, err := gocron.NewScheduler(gocron.WithLocation(time.UTC))
	if err != nil {
		os.Exit(cronSchedulerCreationError)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		_ = logging.Errorf("error creating configuration watcher: %v", err)
		os.Exit(fileWatcherError)
	}
	defer watcher.Close()

	reconcilerConfigWatcher, err := reconciler.NewConfigWatcher(
		reconcilerCronConfiguration,
		s,
		watcher,
		func() {
			reconciler.ReconcileIPs(errorChan)
		},
	)
	if err != nil {
		os.Exit(couldNotCreateConfigWatcherError)
	}
	s.Start()

	const reconcilerConfigMntFile = "/cron-schedule/..data"
	p := func(e fsnotify.Event) bool {
		return e.Name == reconcilerConfigMntFile && e.Op&fsnotify.Create == fsnotify.Create
	}
	reconcilerConfigWatcher.SyncConfiguration(p)

	for {
		select {
		case <-stopChan:
			logging.Verbosef("shutting down reconciler")
			if err := s.Shutdown(); err != nil {
				_ = logging.Errorf("error shutting down scheduler: %v", err)
			}
			return
		case err := <-errorChan:
			if err == nil {
				logging.Verbosef("reconciler run succeeded")
			} else {
				logging.Verbosef("reconciler run failed: %s", err)
			}
		}
	}
}

func handleSignals(stopChannel chan struct{}, signals ...os.Signal) {
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, signals...)
	go func() {
		<-signalChannel
		stopChannel <- struct{}{}
	}()
}
