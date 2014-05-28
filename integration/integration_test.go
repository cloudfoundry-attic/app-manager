package integration_test

import (
	"fmt"
	"time"

	"github.com/cloudfoundry-incubator/app-manager/integration/app_manager_runner"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/yagnats"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Starting apps", func() {
	var (
		natsClient yagnats.NATSClient
		bbs        *Bbs.BBS
	)

	BeforeEach(func() {
		natsClient = natsRunner.MessageBus

		logSink := steno.NewTestingSink()

		steno.Init(&steno.Config{
			Sinks: []steno.Sink{logSink},
		})

		logger := steno.NewLogger("the-logger")
		steno.EnterTestMode()

		bbs = Bbs.NewBBS(etcdRunner.Adapter(), timeprovider.NewTimeProvider(), logger)

		var err error
		var presenceStatus <-chan bool

		fileServerPresence, presenceStatus, err = bbs.MaintainFileServerPresence(time.Second, "http://some.file.server", "file-server-id")
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(presenceStatus).Should(Receive(BeTrue()))

		runner = app_manager_runner.New(
			appManagerPath,
			etcdRunner.NodeURLS(),
			[]string{fmt.Sprintf("127.0.0.1:%d", natsPort)},
			map[string]string{"some-stack": "some-health-check.tar.gz"},
			"127.0.0.1:20515",
		)

		runner.Start()
	})

	AfterEach(func() {
		runner.KillWithFire()
		fileServerPresence.Remove()
	})

	Describe("when a 'diego.desire.app' message is recieved", func() {
		BeforeEach(func() {
			natsClient.Publish("diego.desire.app", []byte(`
        {
          "app_id": "the-app-guid",
          "app_version": "the-app-version",
          "droplet_uri": "http://the-droplet.uri.com",
          "start_command": "the-start-command",
          "memory_mb": 128,
          "disk_mb": 512,
          "file_descriptors": 32,
          "num_instances": 3,
          "stack": "some-stack"
        }
      `))
		})

		Context("for an app that is not running at all", func() {
			It("desires N start auctions in the BBS", func() {
				Eventually(bbs.GetAllLRPStartAuctions, 0.5).Should(HaveLen(3))
			})
		})

		Context("for an app that has some instances", func() {
			BeforeEach(func() {
				bbs.ReportActualLRPAsRunning(models.LRP{
					ProcessGuid:  "the-app-guid-the-app-version",
					InstanceGuid: "a",
					Index:        0,
				})
			})

			It("start auctions for the missing instances", func() {
				Eventually(bbs.GetAllLRPStartAuctions, 0.5).Should(HaveLen(2))
				auctions, err := bbs.GetAllLRPStartAuctions()
				Ω(err).ShouldNot(HaveOccurred())

				indices := []int{auctions[0].Index, auctions[1].Index}
				Ω(indices).Should(ContainElement(1))
				Ω(indices).Should(ContainElement(2))

				Consistently(bbs.GetAllLRPStartAuctions).Should(HaveLen(2))
			})
		})
	})
})
