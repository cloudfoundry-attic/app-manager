package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/cloudfoundry-incubator/app-manager/delta_force"
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

	actualInstances, err := h.actualsForProcessGuid(lrpGuid)
	if err != nil {
		h.logger.Errord(map[string]interface{}{
			"desired-app-message": desireAppMessage,
			"error":               err,
		}, "handler.fetch-actuals.failed")
		return
	}

	delta := delta_force.Reconcile(desireAppMessage.NumInstances, actualInstances)

	for _, lrpIndex := range delta.IndicesToStart {
		h.logger.Infod(map[string]interface{}{
			"desired-app-message": desireAppMessage,
			"index":               lrpIndex,
		}, "handler.request-start")
		startMessage, err := h.startMessageBuilder.Build(desireAppMessage, lrpIndex, fileServerURL)

		if err != nil {
			h.logger.Errord(map[string]interface{}{
				"desired-app-message": desireAppMessage,
				"index":               lrpIndex,
				"error":               err,
			}, "handler.build-start-message.failed")
			continue
		}

		err = h.bbs.RequestLRPStartAuction(startMessage)
		if err != nil {
			h.logger.Errord(map[string]interface{}{
				"desired-app-message": desireAppMessage,
				"index":               lrpIndex,
				"error":               err,
			}, "handler.request-start-auction.failed")
		}
	}

	for _, guidToStop := range delta.GuidsToStop {
		h.logger.Infod(map[string]interface{}{
			"desired-app-message": desireAppMessage,
			"stop-instance-guid":  guidToStop,
		}, "handler.request-stop")
		err = h.bbs.RequestStopLRPInstance(models.StopLRPInstance{InstanceGuid: guidToStop})
		if err != nil {
			h.logger.Errord(map[string]interface{}{
				"desired-app-message": desireAppMessage,
				"stop-instance-guid":  guidToStop,
				"error":               err,
			}, "handler.request-stop-instance.failed")
		}
	}
}

func (h Handler) actualsForProcessGuid(lrpGuid string) (delta_force.ActualInstances, error) {
	actualInstances := delta_force.ActualInstances{}
	actualLRPs, err := h.bbs.GetActualLRPsByProcessGuid(lrpGuid)

	if err != nil {
		return actualInstances, err
	}

	for _, actualLRP := range actualLRPs {
		actualInstances = append(actualInstances, delta_force.ActualInstance{actualLRP.Index, actualLRP.InstanceGuid})
	}

	return actualInstances, err
}
