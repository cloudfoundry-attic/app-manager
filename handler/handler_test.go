package handler_test

import (
	"errors"
	"syscall"

	. "github.com/cloudfoundry-incubator/app-manager/handler"
	"github.com/cloudfoundry-incubator/app-manager/start_message_builder"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/storeadapter"
	"github.com/tedsuo/ifrit"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Handler", func() {
	var (
		startMessageBuilder       *start_message_builder.StartMessageBuilder
		bbs                       *fake_bbs.FakeAppManagerBBS
		logSink                   *steno.TestingSink
		desiredLRP                models.DesiredLRP
		repAddrRelativeToExecutor string
		healthChecks              map[string]string

		handler ifrit.Process
	)

	BeforeEach(func() {
		logSink = steno.NewTestingSink()

		steno.Init(&steno.Config{
			Sinks: []steno.Sink{logSink},
		})

		logger := steno.NewLogger("the-logger")
		steno.EnterTestMode()

		bbs = fake_bbs.NewFakeAppManagerBBS()

		repAddrRelativeToExecutor = "127.0.0.1:20515"

		healthChecks = map[string]string{
			"some-stack": "some-health-check.tgz",
		}

		startMessageBuilder = start_message_builder.New(repAddrRelativeToExecutor, healthChecks, logger)

		handlerRunner := NewHandler(bbs, startMessageBuilder, logger)

		desiredLRP = models.DesiredLRP{
			ProcessGuid:  "the-app-guid-the-app-version",
			Source:       "http://the-droplet.uri.com",
			Stack:        "some-stack",
			StartCommand: "the-start-command",
			Environment: []models.EnvironmentVariable{
				{Key: "foo", Value: "bar"},
				{Key: "VCAP_APPLICATION", Value: "{\"application_name\":\"my-app\"}"},
			},
			MemoryMB:        128,
			DiskMB:          512,
			FileDescriptors: 32,
			Instances:       2,
			Routes:          []string{"route1", "route2"},
			LogGuid:         "the-log-id",
		}
		handler = ifrit.Envoke(handlerRunner)
	})

	AfterEach(func(done Done) {
		handler.Signal(syscall.SIGINT)
		<-handler.Wait()
		close(done)
	})

	Describe("when a desired LRP change message is received", func() {
		JustBeforeEach(func() {
			bbs.DesiredLRPChangeChan <- models.DesiredLRPChange{
				Before: nil,
				After:  &desiredLRP,
			}
		})

		Describe("the happy path", func() {
			BeforeEach(func() {
				bbs.WhenGettingAvailableFileServer = func() (string, error) {
					return "http://file-server.com/", nil
				}
			})

			It("puts a LRPStartAuction in the bbs", func() {
				Eventually(bbs.GetLRPStartAuctions).Should(HaveLen(2))

				startAuctions := bbs.GetLRPStartAuctions()

				firstStartAuction := startAuctions[0]
				Ω(firstStartAuction.ProcessGuid).Should(Equal("the-app-guid-the-app-version"))

				secondStartAuction := startAuctions[1]
				Ω(secondStartAuction.ProcessGuid).Should(Equal("the-app-guid-the-app-version"))
			})

			It("assigns increasing indices for the auction requests", func() {
				Eventually(bbs.GetLRPStartAuctions).Should(HaveLen(2))
				startAuctions := bbs.GetLRPStartAuctions()

				firstStartAuction := startAuctions[0]
				secondStartAuction := startAuctions[1]

				Ω(firstStartAuction.Index).Should(Equal(0))
				Ω(*firstStartAuction.Log.Index).Should(Equal(0))
				Ω(secondStartAuction.Index).Should(Equal(1))
				Ω(*secondStartAuction.Log.Index).Should(Equal(1))
			})
		})

		Context("when file server is not available", func() {
			BeforeEach(func() {
				bbs.WhenGettingAvailableFileServer = func() (string, error) {
					return "", storeadapter.ErrorKeyNotFound
				}
			})

			It("does not put a LRPStartAuction in the bbs", func() {
				Consistently(bbs.GetLRPStartAuctions).Should(BeEmpty())
			})
		})

		Context("when unable to build a start message", func() {
			BeforeEach(func() {
				desiredLRP.Stack = "some-unknown-stack"
			})

			It("does not put a LRPStartAuction in the bbs", func() {
				Consistently(bbs.GetLRPStartAuctions).Should(BeEmpty())
			})
		})

		Context("when there is an error writing a LRPStartAuction to the BBS", func() {
			BeforeEach(func() {
				bbs.LRPStartAuctionErr = errors.New("connection error")
			})

			It("logs an error", func() {
				Eventually(logSink.Records).Should(HaveLen(4))
				Ω(logSink.Records()[1].Message).Should(ContainSubstring("handler.request-start-auction.failed"))
			})
		})

		Context("when there is an error fetching the actual instances", func() {
			BeforeEach(func() {
				bbs.ActualLRPsErr = errors.New("connection error")
			})

			It("does not put a LRPStartAuction in the bbs", func() {
				Consistently(bbs.GetLRPStartAuctions).Should(BeEmpty())
			})
		})

		Context("when there are already instances running for the desired app, but some are missing", func() {
			BeforeEach(func() {
				desiredLRP.Instances = 4
				bbs.Lock()
				bbs.ActualLRPs = []models.ActualLRP{
					{
						ProcessGuid:  "the-app-guid-the-app-version",
						InstanceGuid: "a",
						Index:        0,
						State:        models.ActualLRPStateStarting,
					},
					{
						ProcessGuid:  "the-app-guid-the-app-version",
						InstanceGuid: "b",
						Index:        4,
						State:        models.ActualLRPStateRunning,
					},
					{
						ProcessGuid:  "the-app-guid-the-app-version",
						InstanceGuid: "c",
						Index:        5,
						State:        models.ActualLRPStateRunning,
					},
				}
				bbs.Unlock()
			})

			It("only starts missing ones", func() {
				Eventually(bbs.GetLRPStartAuctions).Should(HaveLen(3))
				startAuctions := bbs.GetLRPStartAuctions()

				Ω(startAuctions[0].Index).Should(Equal(1))
				Ω(startAuctions[1].Index).Should(Equal(2))
				Ω(startAuctions[2].Index).Should(Equal(3))
			})

			It("does not stop extra ones", func() {
				Consistently(bbs.GetStopLRPInstances).Should(BeEmpty())
			})
		})

		Context("when there are extra instances running for the desired app", func() {
			BeforeEach(func() {
				desiredLRP.Instances = 2
				bbs.Lock()
				bbs.ActualLRPs = []models.ActualLRP{
					{
						ProcessGuid:  "the-app-guid-the-app-version",
						InstanceGuid: "a",
						Index:        0,
						State:        models.ActualLRPStateStarting,
					},
					{
						ProcessGuid:  "the-app-guid-the-app-version",
						InstanceGuid: "b",
						Index:        1,
						State:        models.ActualLRPStateStarting,
					},
					{
						ProcessGuid:  "the-app-guid-the-app-version",
						InstanceGuid: "c",
						Index:        2,
						State:        models.ActualLRPStateRunning,
					},
					{
						ProcessGuid:  "the-app-guid-the-app-version",
						InstanceGuid: "d",
						Index:        3,
						State:        models.ActualLRPStateRunning,
					},
				}
				bbs.Unlock()
			})

			It("doesn't start anything", func() {
				Consistently(bbs.GetLRPStartAuctions).Should(BeEmpty())
			})

			It("stops extra ones", func() {
				Eventually(bbs.GetStopLRPInstances).Should(HaveLen(2))
				stopInstances := bbs.GetStopLRPInstances()

				stopInstance1 := models.StopLRPInstance{
					ProcessGuid:  "the-app-guid-the-app-version",
					Index:        2,
					InstanceGuid: "c",
				}
				stopInstance2 := models.StopLRPInstance{
					ProcessGuid:  "the-app-guid-the-app-version",
					Index:        3,
					InstanceGuid: "d",
				}

				Ω(stopInstances).Should(ContainElement(stopInstance1))
				Ω(stopInstances).Should(ContainElement(stopInstance2))
			})
		})
	})

	Describe("when a desired LRP is deleted", func() {
		JustBeforeEach(func() {
			bbs.DesiredLRPChangeChan <- models.DesiredLRPChange{
				Before: &desiredLRP,
				After:  nil,
			}
		})

		BeforeEach(func() {
			bbs.Lock()
			bbs.ActualLRPs = []models.ActualLRP{
				{
					ProcessGuid:  "the-app-guid-the-app-version",
					InstanceGuid: "a",
					Index:        0,
					State:        models.ActualLRPStateStarting,
				},
			}
			bbs.Unlock()
		})

		It("doesn't start anything", func() {
			Consistently(bbs.GetLRPStartAuctions).Should(BeEmpty())
		})

		It("stops all instances", func() {
			Eventually(bbs.GetStopLRPInstances).Should(HaveLen(1))
			stopInstances := bbs.GetStopLRPInstances()

			stopInstance := models.StopLRPInstance{
				ProcessGuid:  "the-app-guid-the-app-version",
				Index:        0,
				InstanceGuid: "a",
			}

			Ω(stopInstances).Should(ContainElement(stopInstance))
		})
	})
})
