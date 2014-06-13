package main

import (
	"encoding/json"
	"flag"
	"os"
	"strings"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/storeadapter/etcdstoreadapter"
	"github.com/cloudfoundry/storeadapter/workerpool"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"

	"github.com/cloudfoundry-incubator/app-manager/handler"
	"github.com/cloudfoundry-incubator/app-manager/start_message_builder"
)

var repAddrRelativeToExecutor = flag.String(
	"repAddrRelativeToExecutor",
	"127.0.0.1:20515",
	"address of the rep server that should receive health status updates",
)

var etcdCluster = flag.String(
	"etcdCluster",
	"http://127.0.0.1:4001",
	"comma-separated list of etcd addresses (http://ip:port)",
)

var syslogName = flag.String(
	"syslogName",
	"",
	"syslog name",
)

var circuses = flag.String(
	"circuses",
	"",
	"app lifecycle binary bundle mapping (stack => bundle filename in fileserver)",
)

func main() {
	flag.Parse()

	logger := initializeLogger()
	bbs := initializeBbs(logger)

	var circuseDownloadURLs map[string]string
	err := json.Unmarshal([]byte(*circuses), &circuseDownloadURLs)
	if err != nil {
		logger.Fatalf("invalid health checks: %s\n", err)
	}

	startMessageBuilder := start_message_builder.New(*repAddrRelativeToExecutor, circuseDownloadURLs, logger)

	group := grouper.EnvokeGroup(grouper.RunGroup{
		"handler": handler.NewHandler(bbs, startMessageBuilder, logger),
	})

	logger.Info("app_manager.started")

	monitor := ifrit.Envoke(sigmon.New(group))

	err = <-monitor.Wait()
	if err != nil {
		logger.Errord(map[string]interface{}{
			"error": err.Error(),
		}, "app_manager.exited")
		os.Exit(1)
	}
	logger.Info("app_manager.exited")
}

func initializeLogger() *steno.Logger {
	stenoConfig := &steno.Config{
		Sinks: []steno.Sink{
			steno.NewIOSink(os.Stdout),
		},
	}

	if *syslogName != "" {
		stenoConfig.Sinks = append(stenoConfig.Sinks, steno.NewSyslogSink(*syslogName))
	}

	steno.Init(stenoConfig)

	return steno.NewLogger("AppManager")
}

func initializeBbs(logger *steno.Logger) Bbs.AppManagerBBS {
	etcdAdapter := etcdstoreadapter.NewETCDStoreAdapter(
		strings.Split(*etcdCluster, ","),
		workerpool.NewWorkerPool(10),
	)

	err := etcdAdapter.Connect()
	if err != nil {
		logger.Fatalf("Error connecting to etcd: %s\n", err)
	}

	return Bbs.NewAppManagerBBS(etcdAdapter, timeprovider.NewTimeProvider(), logger)
}
