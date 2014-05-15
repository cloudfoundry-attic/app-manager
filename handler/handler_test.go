package handler_test

import (
	"encoding/json"
	"errors"
	"fmt"
	. "github.com/cloudfoundry-incubator/app-manager/handler"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/yagnats/fakeyagnats"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Inbox", func() {
	var (
		fakenats         *fakeyagnats.FakeYagnats
		bbs              *fake_bbs.FakeAppManagerBBS
		logSink          *steno.TestingSink
		desireAppRequest models.DesireAppRequestFromCC

		handler Handler
	)

	BeforeEach(func() {
		logSink = steno.NewTestingSink()
		steno.Init(&steno.Config{
			Sinks: []steno.Sink{logSink},
		})
		logger := steno.NewLogger("the-logger")

		fakenats = fakeyagnats.New()
		bbs = fake_bbs.NewFakeAppManagerBBS()
		handler = NewHandler(fakenats, bbs, logger)

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
		}
	})

	Describe("Start", func() {
		BeforeEach(func() {
			handler.Start()
		})

		Describe("when a 'diego.desire.app' message is received", func() {
			JustBeforeEach(func() {
				messagePayload, err := json.Marshal(desireAppRequest)
				Ω(err).ShouldNot(HaveOccurred())

				fakenats.Publish("diego.desire.app", messagePayload)
			})

			It("puts a desired LRP in the BBS for the given app", func() {
				Ω(bbs.DesiredLrps()).Should(HaveLen(1))

				lrp := bbs.DesiredLrps()[0]
				Ω(lrp.Guid).Should(Equal("the-app-guid-the-app-version"))
				Ω(lrp.Stack).Should(Equal("some-stack"))
				Ω(lrp.State).Should(Equal(models.TransitionalLRPStateDesired))
				Ω(lrp.MemoryMB).Should(Equal(128))
				Ω(lrp.DiskMB).Should(Equal(512))

				zero := 0
				numFiles := uint64(32)
				Ω(lrp.Log).Should(Equal(models.LogConfig{
					Guid:       "the-app-guid",
					SourceName: "App",
					Index:      &zero,
				}))

				Ω(lrp.Actions).Should(HaveLen(2))

				Ω(lrp.Actions[0].Action).Should(Equal(models.DownloadAction{
					From:     "http://the-droplet.uri.com",
					To:       ".",
					Extract:  true,
					CacheKey: "droplets-the-app-guid-the-app-version",
				}))

				runAction, ok := lrp.Actions[1].Action.(models.RunAction)
				Ω(ok).Should(BeTrue())

				Ω(runAction.Script).Should(Equal("cd ./app && the-start-command"))
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
				}`, lrp.Guid, *lrp.Log.Index)))
			})

			Describe("when there is an error writing to the BBS", func() {
				BeforeEach(func() {
					bbs.DesireLrpErr = errors.New("connection error")
				})

				It("logs an error", func() {
					Ω(logSink.Records()).Should(HaveLen(1))
					Ω(logSink.Records()[0].Message).Should(ContainSubstring("connection error"))
				})
			})

			Context("when there is no file descriptor limit", func() {
				BeforeEach(func() {
					desireAppRequest.FileDescriptors = 0
				})

				It("does not set any FD limit on the run action", func() {
					Ω(bbs.DesiredLrps()).Should(HaveLen(1))

					lrp := bbs.DesiredLrps()[0]

					Ω(lrp.Actions).Should(HaveLen(2))
					runAction, ok := lrp.Actions[1].Action.(models.RunAction)
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
				Ω(logSink.Records()).Should(HaveLen(1))
				Ω(logSink.Records()[0].Message).Should(ContainSubstring("Failed to parse NATS message."))
			})

			It("does not put an LRP into the BBS", func() {
				Ω(bbs.DesiredLrps()).Should(BeEmpty())
			})
		})
	})
})
