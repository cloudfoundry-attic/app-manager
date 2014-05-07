package handler_test

import (
	"errors"
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
		fakenats *fakeyagnats.FakeYagnats
		bbs      *fake_bbs.FakeAppManagerBBS
		logSink  *steno.TestingSink
		handler  Handler
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
	})

	Describe("Start", func() {
		BeforeEach(func() {
			handler.Start()
		})

		Describe("when a 'diego.desire.app' message is received", func() {
			JustBeforeEach(func() {
				fakenats.Publish("diego.desire.app", []byte(`
          {
            "app_id": "the-app-guid",
            "app_version": "the-app-version",
            "droplet_uri": "http://the-droplet.uri.com",
            "start_command": "the-start-command"
          }
        `))
			})

			It("puts a desired LRP in the BBS for the given app", func() {
				Ω(bbs.DesiredLrps()).Should(HaveLen(1))

				lrp := bbs.DesiredLrps()[0]
				Ω(lrp.Guid).Should(Equal("the-app-guid-the-app-version"))
				Ω(lrp.State).Should(Equal(models.TransitionalLRPStateDesired))

				zero := 0
				Ω(lrp.Log).Should(Equal(models.LogConfig{
					Guid:       "the-app-guid",
					SourceName: "App",
					Index:      &zero,
				}))

				Ω(lrp.Actions).Should(Equal([]models.ExecutorAction{
					{
						Action: models.DownloadAction{
							From:     "http://the-droplet.uri.com",
							To:       "/app",
							Extract:  true,
							CacheKey: "droplets-the-app-guid-the-app-version",
						},
					},
					{
						Action: models.RunAction{
							Script: "cd /app && the-start-command",
						},
					},
				}))
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
