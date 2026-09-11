package e2e_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Kind topology", func() {
	It("declares one control-plane and two workers", func() {
		_, filename, _, ok := runtime.Caller(0)
		Expect(ok).To(BeTrue())

		content, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "kind", "cluster.yaml"))
		Expect(err).NotTo(HaveOccurred())

		cluster := string(content)
		Expect(strings.Count(cluster, "role: control-plane")).To(Equal(1))
		Expect(strings.Count(cluster, "role: worker")).To(Equal(2))
	})
})
