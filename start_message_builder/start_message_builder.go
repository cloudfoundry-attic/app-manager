package start_message_builder

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	RepRoutes "github.com/cloudfoundry-incubator/rep/routes"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	SchemaRouter "github.com/cloudfoundry-incubator/runtime-schema/router"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/gunk/urljoiner"
	"github.com/nu7hatch/gouuid"
	"github.com/tedsuo/router"
)

var ErrNoHealthCheckDefined = errors.New("no health check defined for stack")

type StartMessageBuilder struct {
	repAddrRelativeToExecutor string
	logger                    *steno.Logger
	healthChecks              map[string]string
}

func New(repAddrRelativeToExecutor string, healthChecks map[string]string, logger *steno.Logger) *StartMessageBuilder {
	return &StartMessageBuilder{
		repAddrRelativeToExecutor: repAddrRelativeToExecutor,
		healthChecks:              healthChecks,
		logger:                    logger,
	}
}

func (b *StartMessageBuilder) Build(desireAppMessage models.DesireAppRequestFromCC, lrpIndex int, fileServerURL string) (models.LRPStartAuction, error) {
	lrpGuid := fmt.Sprintf("%s-%s", desireAppMessage.AppId, desireAppMessage.AppVersion)

	instanceGuid, err := uuid.NewV4()
	if err != nil {
		b.logger.Errorf("Error generating instance guid: %s", err.Error())
		return models.LRPStartAuction{}, err
	}

	healthCheckURL, err := b.healthCheckDownloadURL(desireAppMessage.Stack, fileServerURL)
	if err != nil {
		b.logger.Warnd(
			map[string]interface{}{
				"error": err.Error(),
				"stack": desireAppMessage.Stack,
			},
			"handler.construct-health-check-download-url.failed",
		)

		return models.LRPStartAuction{}, err
	}

	lrpEnv, err := createLrpEnv(desireAppMessage.Environment, lrpGuid, lrpIndex)
	if err != nil {
		b.logger.Warnd(
			map[string]interface{}{
				"error": err.Error(),
			},
			"handler.constructing-env.failed",
		)

		return models.LRPStartAuction{}, err
	}

	var numFiles *uint64
	if desireAppMessage.FileDescriptors != 0 {
		numFiles = &desireAppMessage.FileDescriptors
	}

	repRequests := router.NewRequestGenerator(
		"http://"+b.repAddrRelativeToExecutor,
		RepRoutes.Routes,
	)

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
		return models.LRPStartAuction{}, err
	}

	return models.LRPStartAuction{
		ProcessGuid:  lrpGuid,
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
								Script: "/tmp/diego-health-check/diego-health-check -addr=:8080",
							},
						},
						HealthyThreshold:   1,
						UnhealthyThreshold: 1,
						HealthyHook: models.HealthRequest{
							Method: healthyHook.Method,
							URL:    healthyHook.URL.String(),
						},
					},
				},
			),
		},
	}, nil
}

func (b StartMessageBuilder) healthCheckDownloadURL(stack string, fileServerURL string) (*url.URL, error) {
	checkPath, ok := b.healthChecks[stack]
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
