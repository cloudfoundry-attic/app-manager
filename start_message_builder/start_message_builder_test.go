package start_message_builder_test

import (
	"fmt"
	"regexp"
	"strings"
	. "github.com/cloudfoundry-incubator/app-manager/start_message_builder"
	"github.com/cloudfoundry-incubator/runtime-schema/models"

	steno "github.com/cloudfoundry/gosteno"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Start Message Builder", func() {
	var (
		builder                   *StartMessageBuilder
		repAddrRelativeToExecutor string
		desireAppRequest          models.DesireAppRequestFromCC
		healthChecks              map[string]string
		fileServerURL             string
	)

	BeforeEach(func() {
		fileServerURL = "http://file-server.com"
		repAddrRelativeToExecutor = "127.0.0.1:20515"
		logger := steno.NewLogger("the-logger")
		healthChecks = map[string]string{
			"some-stack": "some-health-check.tgz",
		}
		builder = New(repAddrRelativeToExecutor, healthChecks, logger)
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
			NumInstances:    23,
			Routes:          []string{"route1", "route2"},
		}
	})

	It("builds a valid LRPStartAuction", func() {
		auction, err := builder.Build(desireAppRequest, 22, fileServerURL)
		Ω(err).ShouldNot(HaveOccurred())

		Ω(auction.Index).Should(Equal(22))
		Ω(auction.ProcessGuid).Should(Equal("the-app-guid-the-app-version"))
		Ω(auction.InstanceGuid).ShouldNot(BeEmpty())
		Ω(auction.Stack).Should(Equal("some-stack"))
		Ω(auction.State).Should(Equal(models.LRPStartAuctionStatePending))
		Ω(auction.MemoryMB).Should(Equal(128))
		Ω(auction.DiskMB).Should(Equal(512))
		Ω(auction.Ports).Should(Equal([]models.PortMapping{{ContainerPort: 8080}}))

		twentyTwo := 22
		numFiles := uint64(32)
		Ω(auction.Log).Should(Equal(models.LogConfig{
			Guid:       "the-app-guid",
			SourceName: "App",
			Index:      &twentyTwo,
		}))

		Ω(auction.Actions).Should(HaveLen(3))

		Ω(auction.Actions[0].Action).Should(Equal(models.DownloadAction{
			From:    "http://file-server.com/v1/static/some-health-check.tgz",
			To:      "/tmp/diego-health-check",
			Extract: true,
		}))

		Ω(auction.Actions[1].Action).Should(Equal(models.DownloadAction{
			From:     "http://the-droplet.uri.com",
			To:       ".",
			Extract:  true,
			CacheKey: "droplets-the-app-guid-the-app-version",
		}))

		parallelAction, ok := auction.Actions[2].Action.(models.ParallelAction)
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
			URL:    "http://" + repAddrRelativeToExecutor + "/lrp_running/the-app-guid-the-app-version/22/" + auction.InstanceGuid,
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
          }`, auction.ProcessGuid, *auction.Log.Index)))
	})

	It("assigns unique instance guids to the auction requests", func() {
		auction, err := builder.Build(desireAppRequest, 22, fileServerURL)
		Ω(err).ShouldNot(HaveOccurred())

		secondStartAuction, err := builder.Build(desireAppRequest, 22, fileServerURL)
		Ω(err).ShouldNot(HaveOccurred())

		Ω(auction.InstanceGuid).ShouldNot(Equal(secondStartAuction.InstanceGuid))
	})

	Context("when there is no file descriptor limit", func() {
		BeforeEach(func() {
			desireAppRequest.FileDescriptors = 0
		})

		It("does not set any FD limit on the run action", func() {
			auction, err := builder.Build(desireAppRequest, 22, fileServerURL)
			Ω(err).ShouldNot(HaveOccurred())

			Ω(auction.Actions).Should(HaveLen(3))

			parallelAction, ok := auction.Actions[2].Action.(models.ParallelAction)
			Ω(ok).Should(BeTrue())

			runAction, ok := parallelAction.Actions[0].Action.(models.RunAction)
			Ω(ok).Should(BeTrue())

			Ω(runAction.ResourceLimits).Should(Equal(models.ResourceLimits{
				Nofile: nil,
			}))
		})
	})

	Context("when requesting a stack with no associated health-check", func() {
		BeforeEach(func() {
			desireAppRequest.Stack = "some-other-stack"
		})

		It("should error", func() {
			auction, err := builder.Build(desireAppRequest, 22, fileServerURL)
			Ω(err).Should(MatchError(ErrNoHealthCheckDefined))
			Ω(auction).Should(BeZero())
		})
	})
})

func stripWhitespace(input string) string {
	whitespaceRegexp := regexp.MustCompile("\\s+")
	return strings.TrimSpace(whitespaceRegexp.ReplaceAllString(input, " "))
}