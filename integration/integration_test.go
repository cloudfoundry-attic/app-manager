package integration_test

import (
	"time"

	"github.com/cloudfoundry/storeadapter/test_helpers"
	"github.com/pivotal-golang/lager/lagertest"

	"github.com/cloudfoundry-incubator/app-manager/integration/app_manager_runner"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
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

		bbs = Bbs.NewBBS(etcdRunner.Adapter(), timeprovider.NewTimeProvider(), lagertest.NewTestLogger("test"))

		var err error
		var presenceStatus <-chan bool

		fileServerPresence, presenceStatus, err = bbs.MaintainFileServerPresence(time.Second, "http://some.file.server", "file-server-id")
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(presenceStatus).Should(Receive(BeTrue()))

		test_helpers.NewStatusReporter(presenceStatus)

		runner = app_manager_runner.New(
			appManagerPath,
			etcdRunner.NodeURLS(),
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
		JustBeforeEach(func() {
			err := bbs.DesireLRP(models.DesiredLRP{
				ProcessGuid:     "the-guid",
				Source:          "http://the-droplet.uri.com",
				FileDescriptors: 32,
				Environment:     nil,
				StartCommand:    "the-start-command",
				Instances:       3,
				MemoryMB:        128,
				DiskMB:          512,
				Stack:           "some-stack",
				Routes:          nil,
				LogGuid:         "the-log-guid",
			})
			Ω(err).ShouldNot(HaveOccurred())
		})

		It("registers an app desire in etcd", func() {
			Eventually(bbs.GetAllDesiredLRPs).Should(HaveLen(1))
		})

		Context("for an app that is not running at all", func() {
			It("desires N start auctions in the BBS", func() {
				Eventually(bbs.GetAllLRPStartAuctions, 0.5).Should(HaveLen(3))
			})
		})

		Context("for an app that is missing instances", func() {
			BeforeEach(func() {
				bbs.ReportActualLRPAsRunning(models.ActualLRP{
					ProcessGuid:  "the-guid",
					InstanceGuid: "a",
					Index:        0,
				}, "executor-id")
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

		Context("for an app that has extra instances", func() {
			BeforeEach(func() {
				bbs.ReportActualLRPAsRunning(models.ActualLRP{
					ProcessGuid:  "the-guid",
					InstanceGuid: "a",
					Index:        0,
				}, "executor-id")

				bbs.ReportActualLRPAsRunning(models.ActualLRP{
					ProcessGuid:  "the-guid",
					InstanceGuid: "b",
					Index:        1,
				}, "executor-id")

				bbs.ReportActualLRPAsRunning(models.ActualLRP{
					ProcessGuid:  "the-guid",
					InstanceGuid: "c",
					Index:        2,
				}, "executor-id")

				bbs.ReportActualLRPAsRunning(models.ActualLRP{
					ProcessGuid:  "the-guid",
					InstanceGuid: "d-extra",
					Index:        3,
				}, "executor-id")
			})

			It("stops the extra instances", func() {
				Consistently(bbs.GetAllLRPStartAuctions, 0.5).Should(BeEmpty())
				Eventually(bbs.GetAllStopLRPInstances).Should(HaveLen(1))
				stopInstances, err := bbs.GetAllStopLRPInstances()
				Ω(err).ShouldNot(HaveOccurred())

				Ω(stopInstances[0].ProcessGuid).Should(Equal("the-guid"))
				Ω(stopInstances[0].Index).Should(Equal(3))
				Ω(stopInstances[0].InstanceGuid).Should(Equal("d-extra"))
			})
		})

		Context("when an app is no longer desired", func() {
			JustBeforeEach(func() {
				Eventually(bbs.GetAllDesiredLRPs).Should(HaveLen(1))
				err := bbs.RemoveDesiredLRPByProcessGuid("the-guid")
				Ω(err).ShouldNot(HaveOccurred())
			})

			It("should remove the desired state from etcd", func() {
				Eventually(bbs.GetAllDesiredLRPs).Should(HaveLen(0))
			})
		})
	})
})
