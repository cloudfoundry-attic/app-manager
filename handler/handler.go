package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/cloudfoundry-incubator/app-manager/start_message_builder"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/yagnats"
)

const DesireAppTopic = "diego.desire.app"

var ErrNoHealthCheckDefined = errors.New("no health check defined for stack")

type Handler struct {
	natsClient          yagnats.NATSClient
	bbs                 Bbs.AppManagerBBS
	startMessageBuilder *start_message_builder.StartMessageBuilder
	logger              *steno.Logger
}

func NewHandler(
	natsClient yagnats.NATSClient,
	bbs Bbs.AppManagerBBS,
	startMessageBuilder *start_message_builder.StartMessageBuilder,
	logger *steno.Logger,
) Handler {
	return Handler{
		natsClient:          natsClient,
		bbs:                 bbs,
		startMessageBuilder: startMessageBuilder,
		logger:              logger,
	}
}

func (h Handler) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	wg := new(sync.WaitGroup)
	desiredApps := make(chan models.DesireAppRequestFromCC)
	h.listenForDesiredApps(desiredApps)

	close(ready)

	for {
		select {
		case msg := <-desiredApps:
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.desireApp(msg)
			}()
		case <-signals:
			h.natsClient.UnsubscribeAll(DesireAppTopic)
			wg.Wait()
			return nil
		}
	}
}

func (h Handler) listenForDesiredApps(desiredApps chan models.DesireAppRequestFromCC) {
	h.natsClient.Subscribe(DesireAppTopic, func(message *yagnats.Message) {
		desireAppMessage := models.DesireAppRequestFromCC{}
		err := json.Unmarshal(message.Payload, &desireAppMessage)
		if err != nil {
			h.logger.Errorf("Failed to parse NATS message.")
			return
		}

		desiredApps <- desireAppMessage
	})
}

func (h Handler) desireApp(desireAppMessage models.DesireAppRequestFromCC) {
	lrpGuid := fmt.Sprintf("%s-%s", desireAppMessage.AppId, desireAppMessage.AppVersion)

	desiredLRP := models.DesiredLRP{
		ProcessGuid: lrpGuid,
		Instances:   desireAppMessage.NumInstances,
		MemoryMB:    desireAppMessage.MemoryMB,
		DiskMB:      desireAppMessage.DiskMB,
		Stack:       desireAppMessage.Stack,
		Routes:      desireAppMessage.Routes,
	}

	err := h.bbs.DesireLRP(desiredLRP)
	if err != nil {
		h.logger.Errord(
			map[string]interface{}{
				"desired-app-message": desireAppMessage,
				"error":               err.Error(),
			},
			"app-manager.desire-lrp.failed",
		)

		return
	}

	fileServerURL, err := h.bbs.GetAvailableFileServer()
	if err != nil {
		h.logger.Warnd(
			map[string]interface{}{
				"desired-app-message": desireAppMessage,
				"error":               err.Error(),
			},
			"handler.get-available-file-server.failed",
		)

		return
	}

	for index := 0; index < desireAppMessage.NumInstances; index++ {
		startMessage, err := h.startMessageBuilder.Build(desireAppMessage, index, fileServerURL)

		if err != nil {
			h.logger.Errord(map[string]interface{}{
				"desired-app-message": desireAppMessage,
				"index":               index,
				"error":               err,
			}, "handler.build-start-message.failed")
			continue
		}

		err = h.bbs.RequestLRPStartAuction(startMessage)
		if err != nil {
			h.logger.Errord(map[string]interface{}{
				"desired-app-message": desireAppMessage,
				"index":               index,
				"error":               err,
			}, "handler.request-start-auction.failed")
		}
	}
}
