package integration_test

import (
	"fmt"
	"testing"

	"github.com/cloudfoundry/gunk/natsrunner"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/yagnats"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry/storeadapter/storerunner/etcdstorerunner"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/cloudfoundry-incubator/app-manager/integration/app_manager_runner"
)

var appManagerPath string
var etcdRunner *etcdstorerunner.ETCDClusterRunner
var natsRunner *natsrunner.NATSRunner
var runner *app_manager_runner.AppManagerRunner

var _ = Describe("Main", func() {
	var (
		natsClient yagnats.NATSClient
		bbs        *Bbs.BBS
	)

	BeforeEach(func() {
		etcdPort := 5001 + GinkgoParallelNode()
		natsPort := 4001 + GinkgoParallelNode()

		etcdRunner = etcdstorerunner.NewETCDClusterRunner(etcdPort, 1)
		etcdRunner.Start()

		natsRunner = natsrunner.NewNATSRunner(natsPort)
		natsRunner.Start()

		natsClient = natsRunner.MessageBus

		bbs = Bbs.New(etcdRunner.Adapter(), timeprovider.NewTimeProvider())

		runner = app_manager_runner.New(
			appManagerPath,
			[]string{fmt.Sprintf("http://127.0.0.1:%d", etcdPort)},
			[]string{fmt.Sprintf("127.0.0.1:%d", natsPort)},
		)
	})

	AfterEach(func() {
		runner.KillWithFire()
		etcdRunner.Stop()
		natsRunner.Stop()
	})

	Context("when started", func() {
		BeforeEach(func() {
			runner.Start()
		})

		Describe("when a 'diego.desire.app' message is recieved", func() {
			BeforeEach(func() {
				natsClient.Publish("diego.desire.app", []byte(`
					{
	          "app_id": "the-app-guid",
  	        "app_version": "the-app-version",
	          "droplet_uri": "http://the-droplet.uri.com",
    	      "start_command": "the-start-command",
						"memory_mb" : 128,
						"disk_mb" : 512,
						"file_descriptors" : 32
		      }
				`))
			})

			It("desires a long running process in the BBS", func() {
				Eventually(bbs.GetAllTransitionalLongRunningProcesses, 0.5).Should(HaveLen(1))
			})
		})
	})
})

func TestAppManagerMain(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Integration Suite")
}

var _ = BeforeSuite(func() {
	var err error
	appManagerPath, err = gexec.Build("github.com/cloudfoundry-incubator/app-manager", "-race")
	Ω(err).ShouldNot(HaveOccurred())
})

var _ = AfterSuite(func() {
	gexec.CleanupBuildArtifacts()
	if etcdRunner != nil {
		etcdRunner.Stop()
	}
	if natsRunner != nil {
		natsRunner.Stop()
	}
	if runner != nil {
		runner.KillWithFire()
	}
})
