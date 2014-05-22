package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/storeadapter/etcdstoreadapter"
	"github.com/cloudfoundry/storeadapter/workerpool"
	"github.com/cloudfoundry/yagnats"
	"github.com/tedsuo/ifrit"

	"github.com/cloudfoundry-incubator/app-manager/handler"
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

var natsAddresses = flag.String(
	"natsAddresses",
	"127.0.0.1:4222",
	"comma-separated list of NATS addresses (ip:port)",
)

var natsUsername = flag.String(
	"natsUsername",
	"nats",
	"Username to connect to nats",
)

var natsPassword = flag.String(
	"natsPassword",
	"nats",
	"Password for nats user",
)

var syslogName = flag.String(
	"syslogName",
	"",
	"syslog name",
)

var healthChecks = flag.String(
	"healthChecks",
	"",
	"health check mapping (stack => health check filename in fileserver)",
)

func main() {
	flag.Parse()

	logger := initializeLogger()
	natsClient := initializeNatsClient(logger)
	bbs := initializeBbs(logger)

	var healthCheckDownloadURLs map[string]string
	err := json.Unmarshal([]byte(*healthChecks), &healthCheckDownloadURLs)
	if err != nil {
		log.Fatalln("invalid health checks:", err)
	}

	appManager := ifrit.Envoke(handler.NewHandler(*repAddrRelativeToExecutor, healthCheckDownloadURLs, natsClient, bbs, logger))

	logger.Infof("app_manager.started")

	sigChan := make(chan os.Signal, 1)

	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case sig := <-sigChan:
			appManager.Signal(sig)
		case err := <-appManager.Wait():
			if err != nil {
				logger.Errord(map[string]interface{}{
					"error": err.Error(),
				}, "app_manager.exited")
				return
			}
			logger.Infof("app_manager.exited")
			return
		}
	}
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

func initializeNatsClient(logger *steno.Logger) yagnats.NATSClient {
	natsClient := yagnats.NewClient()

	natsMembers := []yagnats.ConnectionProvider{}
	for _, addr := range strings.Split(*natsAddresses, ",") {
		natsMembers = append(
			natsMembers,
			&yagnats.ConnectionInfo{addr, *natsUsername, *natsPassword},
		)
	}

	err := natsClient.Connect(&yagnats.ConnectionCluster{
		Members: natsMembers,
	})

	if err != nil {
		logger.Fatalf("Error connecting to NATS: %s\n", err)
	}

	return natsClient
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

	return Bbs.NewAppManagerBBS(etcdAdapter, timeprovider.NewTimeProvider())
}
