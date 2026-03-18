package udev_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestUdev(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Udev Suite")
}
