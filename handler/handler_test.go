package handler_test

import (
	"encoding/json"
	"errors"
	"syscall"

	. "github.com/cloudfoundry-incubator/app-manager/handler"
	"github.com/cloudfoundry-incubator/app-manager/start_message_builder"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/storeadapter"
	"github.com/cloudfoundry/yagnats/fakeyagnats"
	"github.com/tedsuo/ifrit"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Handler", func() {
	var (
		fakenats                  *fakeyagnats.FakeYagnats
		startMessageBuilder       *start_message_builder.StartMessageBuilder
		bbs                       *fake_bbs.FakeAppManagerBBS
		logSink                   *steno.TestingSink
		desireAppRequest          models.DesireAppRequestFromCC
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

		fakenats = fakeyagnats.New()

		bbs = fake_bbs.NewFakeAppManagerBBS()

		repAddrRelativeToExecutor = "127.0.0.1:20515"

		healthChecks = map[string]string{
			"some-stack": "some-health-check.tgz",
		}

		startMessageBuilder = start_message_builder.New(repAddrRelativeToExecutor, healthChecks, logger)

		handlerRunner := NewHandler(fakenats, bbs, startMessageBuilder, logger)

		desireAppRequest = models.DesireAppRequestFromCC{
			AppId:        "the-app-guid",
			AppVersion:   "the-app-version",
			DropletUri:   "http://the-droplet.uri.com",
			Stack:        "some-stack",
			StartCommand: "the-start-command",
			Environment: []models.EnvironmentVariable{
				{Key: "foo", Value: "bar"},
				{Key: "VCAP_APPLICATION", Value: "{\"application_name\":\"my-app\"}"},
			},
			MemoryMB:        128,
			DiskMB:          512,
			FileDescriptors: 32,
			NumInstances:    2,
			Routes:          []string{"route1", "route2"},
		}
		handler = ifrit.Envoke(handlerRunner)
	})

	AfterEach(func(done Done) {
		handler.Signal(syscall.SIGINT)
		<-handler.Wait()
		close(done)
	})

	Describe("when a 'diego.desire.app' message is received", func() {
		JustBeforeEach(func() {
			messagePayload, err := json.Marshal(desireAppRequest)
			Ω(err).ShouldNot(HaveOccurred())

			fakenats.Publish("diego.desire.app", messagePayload)
		})

		Describe("the happy path", func() {
			BeforeEach(func() {
				bbs.WhenGettingAvailableFileServer = func() (string, error) {
					return "http://file-server.com/", nil
				}
			})

			It("marks the LRP desired in the bbs", func() {
				Eventually(bbs.DesiredLRPs).Should(ContainElement(models.DesiredLRP{
					ProcessGuid: "the-app-guid-the-app-version",
					Instances:   2,
					MemoryMB:    128,
					DiskMB:      512,
					Stack:       "some-stack",
					Routes:      []string{"route1", "route2"},
				}))
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

		Context("when marking the LRP as desired fails", func() {
			BeforeEach(func() {
				bbs.DesireLRPErr = errors.New("oh no!")
			})

			It("does not put a LRPStartAuction in the bbs", func() {
				Consistently(bbs.GetLRPStartAuctions).Should(BeEmpty())
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
				desireAppRequest.Stack = "some-unknown-stack"
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
				desireAppRequest.NumInstances = 4
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

		Context("when there are extra instanes running for the desired app", func() {
			BeforeEach(func() {
				desireAppRequest.NumInstances = 2
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

				stopInstance1 := models.StopLRPInstance{InstanceGuid: "c"}
				stopInstance2 := models.StopLRPInstance{InstanceGuid: "d"}

				Ω(stopInstances).Should(ContainElement(stopInstance1))
				Ω(stopInstances).Should(ContainElement(stopInstance2))
			})
		})
	})

	Describe("when a invalid 'diego.desire.app' message is received", func() {
		BeforeEach(func() {
			fakenats.Publish("diego.desire.app", []byte(`
        {
          "some_random_key": "does not matter"
      `))
		})

		It("logs an error", func() {
			Eventually(logSink.Records).ShouldNot(HaveLen(0))
			Ω(logSink.Records()[0].Message).Should(ContainSubstring("Failed to parse NATS message."))
		})

		It("does not put an LRP into the BBS", func() {
			Consistently(bbs.GetLRPStartAuctions).Should(BeEmpty())
		})
	})
})
