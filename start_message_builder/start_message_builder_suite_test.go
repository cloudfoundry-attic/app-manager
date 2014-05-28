package start_message_builder_test

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"testing"
)

func TestStartMessageBuilder(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "StartMessageBuilder Suite")
}
