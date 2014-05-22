package handler_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall"

	. "github.com/cloudfoundry-incubator/app-manager/handler"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/storeadapter"
	"github.com/cloudfoundry/yagnats/fakeyagnats"
	"github.com/tedsuo/ifrit"

	"regexp"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Handler", func() {
	var (
		fakenats                  *fakeyagnats.FakeYagnats
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

		handlerRunner := NewHandler(repAddrRelativeToExecutor, healthChecks, fakenats, bbs, logger)

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

		Context("when file the server is available", func() {
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

				Ω(firstStartAuction.Index).Should(Equal(0))
				Ω(firstStartAuction.Guid).Should(Equal("the-app-guid-the-app-version"))
				Ω(firstStartAuction.InstanceGuid).ShouldNot(BeEmpty())
				Ω(firstStartAuction.Stack).Should(Equal("some-stack"))
				Ω(firstStartAuction.State).Should(Equal(models.LRPStartAuctionStatePending))
				Ω(firstStartAuction.MemoryMB).Should(Equal(128))
				Ω(firstStartAuction.DiskMB).Should(Equal(512))
				Ω(firstStartAuction.Ports).Should(Equal([]models.PortMapping{{ContainerPort: 8080}}))

				zero := 0
				numFiles := uint64(32)
				Ω(firstStartAuction.Log).Should(Equal(models.LogConfig{
					Guid:       "the-app-guid",
					SourceName: "App",
					Index:      &zero,
				}))

				Ω(firstStartAuction.Actions).Should(HaveLen(3))

				Ω(firstStartAuction.Actions[0].Action).Should(Equal(models.DownloadAction{
					From:    "http://file-server.com/v1/static/some-health-check.tgz",
					To:      "/tmp/diego-health-check",
					Extract: true,
				}))

				Ω(firstStartAuction.Actions[1].Action).Should(Equal(models.DownloadAction{
					From:     "http://the-droplet.uri.com",
					To:       ".",
					Extract:  true,
					CacheKey: "droplets-the-app-guid-the-app-version",
				}))

				parallelAction, ok := firstStartAuction.Actions[2].Action.(models.ParallelAction)
				Ω(ok).Should(BeTrue())

				runAction, ok := parallelAction.Actions[0].Action.(models.RunAction)
				Ω(ok).Should(BeTrue())

				monitorAction, ok := parallelAction.Actions[1].Action.(models.MonitorAction)
				Ω(ok).Should(BeTrue())

				Ω(monitorAction.Action.Action).Should(Equal(models.RunAction{
					Script: "/tmp/diego-health-check/diego-health-check -addr=:8080",
				}))

				Ω(monitorAction.HealthyHook).Should(Equal(models.HealthRequest{
					Method: "PUT",
					URL:    "http://" + repAddrRelativeToExecutor + "/lrp_running/the-app-guid-the-app-version/0/" + firstStartAuction.InstanceGuid,
				}))

				Ω(monitorAction.HealthyThreshold).ShouldNot(BeZero())
				Ω(monitorAction.UnhealthyThreshold).ShouldNot(BeZero())

				Ω(runAction.Script).Should(Equal(stripWhitespace(`
						cd ./app &&
						if [ -d .profile.d ];
						then
							source .profile.d/*.sh;
						fi &&
						the-start-command
					`)))

				Ω(runAction.ResourceLimits).Should(Equal(models.ResourceLimits{
					Nofile: &numFiles,
				}))

				Ω(runAction.Env).Should(ContainElement(models.EnvironmentVariable{
					Key:   "foo",
					Value: "bar",
				}))

				Ω(runAction.Env).Should(ContainElement(models.EnvironmentVariable{
					Key:   "PORT",
					Value: "8080",
				}))

				Ω(runAction.Env).Should(ContainElement(models.EnvironmentVariable{
					Key:   "VCAP_APP_PORT",
					Value: "8080",
				}))

				Ω(runAction.Env).Should(ContainElement(models.EnvironmentVariable{
					Key:   "VCAP_APP_HOST",
					Value: "0.0.0.0",
				}))

				Ω(runAction.Env).Should(ContainElement(models.EnvironmentVariable{
					Key:   "TMPDIR",
					Value: "$HOME/tmp",
				}))

				var vcapAppEnv string
				for _, envVar := range runAction.Env {
					if envVar.Key == "VCAP_APPLICATION" {
						vcapAppEnv = envVar.Value
					}
				}

				Ω(vcapAppEnv).Should(MatchJSON(fmt.Sprintf(`{
						"application_name": "my-app",
						"host":             "0.0.0.0",
						"port":             8080,
						"instance_id":      "%s",
						"instance_index":   %d
					}`, firstStartAuction.Guid, *firstStartAuction.Log.Index)))

				secondStartAuction := startAuctions[1]
				Ω(secondStartAuction.Index).Should(Equal(1))
				Ω(secondStartAuction.InstanceGuid).ShouldNot(BeEmpty())
			})

			It("assigns unique instance guids to the auction requests", func() {
				Eventually(bbs.GetLRPStartAuctions).Should(HaveLen(2))
				startAuctions := bbs.GetLRPStartAuctions()

				firstStartAuction := startAuctions[0]
				secondStartAuction := startAuctions[1]

				Ω(firstStartAuction.InstanceGuid).ShouldNot(Equal(secondStartAuction.InstanceGuid))
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

			Context("when marking the LRP as desired fails", func() {
				BeforeEach(func() {
					bbs.DesireLRPErr = errors.New("oh no!")
				})

				It("does not put a LRPStartAuction in the bbs", func() {
					Consistently(bbs.GetLRPStartAuctions).Should(BeEmpty())
				})
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

		Context("when there is an error writing a LRPStartAuction to the BBS", func() {
			BeforeEach(func() {
				bbs.LRPStartAuctionErr = errors.New("connection error")
			})

			It("logs an error", func() {
				Eventually(logSink.Records).Should(HaveLen(2))
				Ω(logSink.Records()[0].Message).Should(ContainSubstring("connection error"))
			})
		})

		Context("when there is no file descriptor limit", func() {
			BeforeEach(func() {
				desireAppRequest.FileDescriptors = 0
			})

			It("does not set any FD limit on the run action", func() {
				Eventually(bbs.GetLRPStartAuctions).ShouldNot(HaveLen(0))
				startAuctions := bbs.GetLRPStartAuctions()
				startAuction := startAuctions[0]

				Ω(startAuction.Actions).Should(HaveLen(3))

				parallelAction, ok := startAuction.Actions[2].Action.(models.ParallelAction)
				Ω(ok).Should(BeTrue())

				runAction, ok := parallelAction.Actions[0].Action.(models.RunAction)
				Ω(ok).Should(BeTrue())

				Ω(runAction.ResourceLimits).Should(Equal(models.ResourceLimits{
					Nofile: nil,
				}))
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

func stripWhitespace(input string) string {
	whitespaceRegexp := regexp.MustCompile("\\s+")
	return strings.TrimSpace(whitespaceRegexp.ReplaceAllString(input, " "))
}
