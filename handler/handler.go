package handler

import (
	"encoding/json"
	"fmt"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/yagnats"
)

type Handler struct {
	natsClient yagnats.NATSClient
	bbs        Bbs.AppManagerBBS
	logger     *steno.Logger
}

func NewHandler(natsClient yagnats.NATSClient, bbs Bbs.AppManagerBBS, logger *steno.Logger) Handler {
	return Handler{
		natsClient: natsClient,
		bbs:        bbs,
		logger:     logger,
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
		lrpIndex := 0

		var numFiles *uint64
		if desireAppMessage.FileDescriptors != 0 {
			numFiles = &desireAppMessage.FileDescriptors
		}

		lrpEnv, err := createLrpEnv(desireAppMessage.Environment, lrpGuid, lrpIndex)
		if err != nil {

		}

		for index := 0; index < desireAppMessage.NumInstances; index++ {
			err = h.bbs.RequestLRPStartAuction(models.LRPStartAuction{
				Guid:  lrpGuid,
				State: models.LRPStartAuctionStatePending,
				Index: index,

				MemoryMB: desireAppMessage.MemoryMB,
				DiskMB:   desireAppMessage.DiskMB,

				Stack: desireAppMessage.Stack,
				Log: models.LogConfig{
					Guid:       desireAppMessage.AppId,
					SourceName: "App",
					Index:      &lrpIndex,
				},
				Actions: []models.ExecutorAction{
					{
						Action: models.DownloadAction{
							From:     desireAppMessage.DropletUri,
							To:       ".",
							Extract:  true,
							CacheKey: fmt.Sprintf("droplets-%s", lrpGuid),
						},
					},
					{
						Action: models.RunAction{
							Script:  fmt.Sprintf("cd ./app && %s", desireAppMessage.StartCommand),
							Env:     lrpEnv,
							Timeout: 0,
							ResourceLimits: models.ResourceLimits{
								Nofile: numFiles,
							},
						},
					},
				},
			})
			if err != nil {
				h.logger.Errorf("Error writing to BBS: %s", err.Error())
			}
		}

		err = h.bbs.DesireTransitionalLongRunningProcess(models.TransitionalLongRunningProcess{
			Guid:  lrpGuid,
			State: models.TransitionalLRPStateDesired,

			MemoryMB: desireAppMessage.MemoryMB,
			DiskMB:   desireAppMessage.DiskMB,

			Stack: desireAppMessage.Stack,
			Log: models.LogConfig{
				Guid:       desireAppMessage.AppId,
				SourceName: "App",
				Index:      &lrpIndex,
			},
			Actions: []models.ExecutorAction{
				{
					Action: models.DownloadAction{
						From:     desireAppMessage.DropletUri,
						To:       ".",
						Extract:  true,
						CacheKey: fmt.Sprintf("droplets-%s", lrpGuid),
					},
				},
				{
					Action: models.RunAction{
						Script:  fmt.Sprintf("cd ./app && %s", desireAppMessage.StartCommand),
						Env:     lrpEnv,
						Timeout: 0,
						ResourceLimits: models.ResourceLimits{
							Nofile: numFiles,
						},
					},
				},
			},
		})

		if err != nil {
			h.logger.Errorf("Error writing to BBS: %s", err.Error())
		}
	})
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
