package handler

import (
	"errors"
	"os"
	"sync"

	"github.com/cloudfoundry-incubator/app-manager/delta_force"
	"github.com/cloudfoundry-incubator/app-manager/start_message_builder"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
)

var ErrNoHealthCheckDefined = errors.New("no health check defined for stack")

type Handler struct {
	bbs                 Bbs.AppManagerBBS
	startMessageBuilder *start_message_builder.StartMessageBuilder
	logger              *steno.Logger
}

func NewHandler(
	bbs Bbs.AppManagerBBS,
	startMessageBuilder *start_message_builder.StartMessageBuilder,
	logger *steno.Logger,
) Handler {
	return Handler{
		bbs:                 bbs,
		startMessageBuilder: startMessageBuilder,
		logger:              logger,
	}
}

func (h Handler) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	wg := new(sync.WaitGroup)
	desiredChangeChan, stopChan, errChan := h.bbs.WatchForDesiredLRPChanges()

	close(ready)

	for {
		if desiredChangeChan == nil {
			desiredChangeChan, stopChan, errChan = h.bbs.WatchForDesiredLRPChanges()
		}
		select {
		case desiredChange, ok := <-desiredChangeChan:
			if ok {
				wg.Add(1)
				go func() {
					defer wg.Done()
					h.processDesiredChange(desiredChange)
				}()
			} else {
				h.logger.Error("app-manager.handler.watch-closed")
				desiredChangeChan = nil
			}
		case err, ok := <-errChan:
			if ok {
				h.logger.Errord(map[string]interface{}{
					"error": err.Error(),
				}, "app-manager.handler.received-watch-error")
			}
			desiredChangeChan = nil

		case <-signals:
			h.logger.Info("app-manager.handler.shutting-down")
			close(stopChan)
			wg.Wait()
			h.logger.Info("app-manager.handler.shut-down")
			return nil
		}
	}

	return nil
}

func (h Handler) processDesiredChange(desiredChange models.DesiredLRPChange) {
	var desiredLRP models.DesiredLRP
	var desiredInstances int

	if desiredChange.After == nil {
		desiredLRP = *desiredChange.Before
		desiredInstances = 0
	} else {
		desiredLRP = *desiredChange.After
		desiredInstances = desiredLRP.Instances
	}

	fileServerURL, err := h.bbs.GetAvailableFileServer()
	if err != nil {
		h.logger.Warnd(
			map[string]interface{}{
				"desired-app-message": desiredLRP,
				"error":               err.Error(),
			},
			"handler.get-available-file-server.failed",
		)

		return
	}

	actualInstances, instanceGuidToActual, err := h.actualsForProcessGuid(desiredLRP.ProcessGuid)
	if err != nil {
		h.logger.Errord(map[string]interface{}{
			"desired-app-message": desiredLRP,
			"error":               err,
		}, "handler.fetch-actuals.failed")
		return
	}

	delta := delta_force.Reconcile(desiredInstances, actualInstances)

	for _, lrpIndex := range delta.IndicesToStart {
		h.logger.Infod(map[string]interface{}{
			"desired-app-message": desiredLRP,
			"index":               lrpIndex,
		}, "handler.request-start")

		startMessage, err := h.startMessageBuilder.Build(desiredLRP, lrpIndex, fileServerURL)

		if err != nil {
			h.logger.Errord(map[string]interface{}{
				"desired-app-message": desiredLRP,
				"index":               lrpIndex,
				"error":               err,
			}, "handler.build-start-message.failed")
			continue
		}

		err = h.bbs.RequestLRPStartAuction(startMessage)

		if err != nil {
			h.logger.Errord(map[string]interface{}{
				"desired-app-message": desiredLRP,
				"index":               lrpIndex,
				"error":               err,
			}, "handler.request-start-auction.failed")
		}
	}

	for _, guidToStop := range delta.GuidsToStop {
		h.logger.Infod(map[string]interface{}{
			"desired-app-message": desiredLRP,
			"stop-instance-guid":  guidToStop,
		}, "handler.request-stop")

		actualToStop := instanceGuidToActual[guidToStop]

		err = h.bbs.RequestStopLRPInstance(models.StopLRPInstance{
			ProcessGuid:  actualToStop.ProcessGuid,
			InstanceGuid: actualToStop.InstanceGuid,
			Index:        actualToStop.Index,
		})

		if err != nil {
			h.logger.Errord(map[string]interface{}{
				"desired-app-message": desiredLRP,
				"stop-instance-guid":  guidToStop,
				"error":               err,
			}, "handler.request-stop-instance.failed")
		}
	}
}

func (h Handler) actualsForProcessGuid(lrpGuid string) (delta_force.ActualInstances, map[string]models.ActualLRP, error) {
	actualInstances := delta_force.ActualInstances{}
	actualLRPs, err := h.bbs.GetActualLRPsByProcessGuid(lrpGuid)
	instanceGuidToActual := map[string]models.ActualLRP{}

	if err != nil {
		return actualInstances, instanceGuidToActual, err
	}

	for _, actualLRP := range actualLRPs {
		actualInstances = append(actualInstances, delta_force.ActualInstance{actualLRP.Index, actualLRP.InstanceGuid})
		instanceGuidToActual[actualLRP.InstanceGuid] = actualLRP
	}

	return actualInstances, instanceGuidToActual, err
}
