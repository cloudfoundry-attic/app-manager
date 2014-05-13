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
		desireAppMessage := DesireAppNATSMessage{}
		err := json.Unmarshal(message.Payload, &desireAppMessage)
		if err != nil {
			h.logger.Errorf("Failed to parse NATS message.")
			return
		}

		lrpGuid := fmt.Sprintf("%s-%s", desireAppMessage.AppId, desireAppMessage.AppVersion)
		lrpIndex := 0

		err = h.bbs.DesireTransitionalLongRunningProcess(models.TransitionalLongRunningProcess{
			Guid:  lrpGuid,
			State: models.TransitionalLRPStateDesired,
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
						Env:     nil,
						Timeout: 0,
					},
				},
			},
		})

		if err != nil {
			h.logger.Errorf("Error writing to BBS: %s", err.Error())
		}
	})
}

type DesireAppNATSMessage struct {
	AppId        string `json:"app_id"`
	AppVersion   string `json:"app_version"`
	DropletUri   string `json:"droplet_uri"`
	StartCommand string `json:"start_command"`
}
