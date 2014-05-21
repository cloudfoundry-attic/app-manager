package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	RepRoutes "github.com/cloudfoundry-incubator/rep/routes"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	SchemaRouter "github.com/cloudfoundry-incubator/runtime-schema/router"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/gunk/urljoiner"
	"github.com/cloudfoundry/yagnats"
	"github.com/nu7hatch/gouuid"
	"github.com/tedsuo/router"
)

var ErrNoHealthCheckDefined = errors.New("no health check defined for stack")

type Handler struct {
	repAddrRelativeToExecutor string
	healthChecks              map[string]string
	natsClient                yagnats.NATSClient
	bbs                       Bbs.AppManagerBBS
	logger                    *steno.Logger
}

func NewHandler(
	repAddrRelativeToExecutor string,
	healthChecks map[string]string,
	natsClient yagnats.NATSClient,
	bbs Bbs.AppManagerBBS,
	logger *steno.Logger,
) Handler {
	return Handler{
		repAddrRelativeToExecutor: repAddrRelativeToExecutor,
		healthChecks:              healthChecks,
		natsClient:                natsClient,
		bbs:                       bbs,
		logger:                    logger,
	}
}

func (h Handler) Start() {
	h.natsClient.Subscribe("diego.desire.app", func(message *yagnats.Message) {
		desireAppMessage := models.DesireAppRequestFromCC{}
		err := json.Unmarshal(message.Payload, &desireAppMessage)
		if err != nil {
			h.logger.Errorf("Failed to parse NATS message.")
			return
		}

		lrpGuid := fmt.Sprintf("%s-%s", desireAppMessage.AppId, desireAppMessage.AppVersion)

		desiredLRP := models.DesiredLRP{
			ProcessGuid: lrpGuid,
			Instances:   desireAppMessage.NumInstances,
			MemoryMB:    desireAppMessage.MemoryMB,
			DiskMB:      desireAppMessage.DiskMB,
			Stack:       desireAppMessage.Stack,
			Routes:      desireAppMessage.Routes,
		}

		err = h.bbs.DesireLongRunningProcess(desiredLRP)
		if err != nil {
			h.logger.Errord(
				map[string]interface{}{
					"error": err.Error(),
				},
				"app-manager.desire-lrp.failed",
			)

			return
		}

		var numFiles *uint64
		if desireAppMessage.FileDescriptors != 0 {
			numFiles = &desireAppMessage.FileDescriptors
		}

		fileServerURL, err := h.bbs.GetAvailableFileServer()
		if err != nil {
			h.logger.Warnd(
				map[string]interface{}{
					"error": err.Error(),
				},
				"handler.get-available-file-server.failed",
			)

			return
		}

		healthCheckURL, err := h.healthCheckDownloadURL(desireAppMessage.Stack, fileServerURL)
		if err != nil {
			h.logger.Warnd(
				map[string]interface{}{
					"error": err.Error(),
					"stack": desireAppMessage.Stack,
				},
				"handler.construct-health-check-download-url.failed",
			)

			return
		}

		repRequests := router.NewRequestGenerator(
			"http://"+h.repAddrRelativeToExecutor,
			RepRoutes.Routes,
		)

		for index := 0; index < desireAppMessage.NumInstances; index++ {
			lrpIndex := index

			instanceGuid, err := uuid.NewV4()
			if err != nil {
				h.logger.Errorf("Error generating instance guid: %s", err.Error())
				continue
			}

			lrpEnv, err := createLrpEnv(desireAppMessage.Environment, lrpGuid, lrpIndex)
			if err != nil {
				h.logger.Warnd(
					map[string]interface{}{
						"error": err.Error(),
					},
					"handler.constructing-env.failed",
				)

				return
			}

			healthyHook, err := repRequests.RequestForHandler(
				RepRoutes.LRPRunning,
				router.Params{
					"process_guid":  lrpGuid,
					"index":         fmt.Sprintf("%d", lrpIndex),
					"instance_guid": instanceGuid.String(),
				},
				nil,
			)
			if err != nil {
				panic(err)
			}

			err = h.bbs.RequestLRPStartAuction(models.LRPStartAuction{
				Guid:         lrpGuid,
				InstanceGuid: instanceGuid.String(),
				State:        models.LRPStartAuctionStatePending,
				Index:        lrpIndex,

				MemoryMB: desireAppMessage.MemoryMB,
				DiskMB:   desireAppMessage.DiskMB,

				Ports: []models.PortMapping{
					{ContainerPort: 8080},
				},

				Stack: desireAppMessage.Stack,
				Log: models.LogConfig{
					Guid:       desireAppMessage.AppId,
					SourceName: "App",
					Index:      &lrpIndex,
				},
				Actions: []models.ExecutorAction{
					{
						Action: models.DownloadAction{
							From:    healthCheckURL.String(),
							To:      "/tmp/diego-health-check",
							Extract: true,
						},
					},
					{
						Action: models.DownloadAction{
							From:     desireAppMessage.DropletUri,
							To:       ".",
							Extract:  true,
							CacheKey: fmt.Sprintf("droplets-%s", lrpGuid),
						},
					},
					models.Parallel(
						models.ExecutorAction{
							models.RunAction{
								Script: strings.Join([]string{
									"cd ./app",
									"if [ -d .profile.d ]; then source .profile.d/*.sh; fi",
									desireAppMessage.StartCommand,
								}, " && "),
								Env:     lrpEnv,
								Timeout: 0,
								ResourceLimits: models.ResourceLimits{
									Nofile: numFiles,
								},
							},
						},
						models.ExecutorAction{
							models.MonitorAction{
								Action: models.ExecutorAction{
									models.RunAction{
										Script: "/tmp/diego-health-check -addr=:8080",
									},
								},
								HealthyHook: models.HealthRequest{
									Method: healthyHook.Method,
									URL:    healthyHook.URL.String(),
								},
							},
						},
					),
				},
			})
			if err != nil {
				h.logger.Errorf("Error writing to BBS: %s", err.Error())
			}
		}
	})
}

func (h Handler) healthCheckDownloadURL(stack string, fileServerURL string) (*url.URL, error) {
	checkPath, ok := h.healthChecks[stack]
	if !ok {
		return nil, ErrNoHealthCheckDefined
	}

	staticRoute, ok := SchemaRouter.NewFileServerRoutes().RouteForHandler(SchemaRouter.FS_STATIC)
	if !ok {
		return nil, errors.New("couldn't generate the compiler download path")
	}

	urlString := urljoiner.Join(fileServerURL, staticRoute.Path, checkPath)

	url, err := url.ParseRequestURI(urlString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse compiler download URL: %s", err)
	}

	return url, nil
}

func createLrpEnv(env []models.EnvironmentVariable, lrpGuid string, lrpIndex int) ([]models.EnvironmentVariable, error) {
	env = append(env, models.EnvironmentVariable{Key: "PORT", Value: "8080"})
	env = append(env, models.EnvironmentVariable{Key: "VCAP_APP_PORT", Value: "8080"})
	env = append(env, models.EnvironmentVariable{Key: "VCAP_APP_HOST", Value: "0.0.0.0"})
	env = append(env, models.EnvironmentVariable{Key: "TMPDIR", Value: "$HOME/tmp"})

	vcapAppEnv := map[string]interface{}{}
	vcapAppEnvIndex := -1
	for i, envVar := range env {
		if envVar.Key == "VCAP_APPLICATION" {
			vcapAppEnvIndex = i
			err := json.Unmarshal([]byte(envVar.Value), &vcapAppEnv)
			if err != nil {
				return env, err
			}
		}
	}

	if vcapAppEnvIndex == -1 {
		return env, nil
	}

	vcapAppEnv["port"] = 8080
	vcapAppEnv["host"] = "0.0.0.0"
	vcapAppEnv["instance_id"] = lrpGuid
	vcapAppEnv["instance_index"] = lrpIndex

	lrpEnv, err := json.Marshal(vcapAppEnv)
	if err != nil {
		return env, err
	}

	env[vcapAppEnvIndex] = models.EnvironmentVariable{Key: "VCAP_APPLICATION", Value: string(lrpEnv)}
	return env, nil
}
