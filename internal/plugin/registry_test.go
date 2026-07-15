package plugin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("registry probe endpoints", func() {
	var (
		ctx      context.Context
		cancel   context.CancelFunc
		registry *Registry
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())
		registry = &Registry{ctx: ctx}
	})

	AfterEach(func() {
		cancel()
	})

	request := func(handler http.HandlerFunc) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		return recorder
	}

	It("keeps constant-time liveness healthy during initialization", func() {
		Expect(request(registry.Healthz).Code).To(Equal(http.StatusOK))
	})

	It("keeps readiness false until initialization completes", func() {
		response := request(registry.Readyz)
		Expect(response.Code).To(Equal(http.StatusServiceUnavailable))
		Expect(response.Body.String()).To(ContainSubstring("still in progress"))

		registry.SetInitialized(nil)
		Expect(request(registry.Readyz).Code).To(Equal(http.StatusOK))
	})

	It("keeps readiness false after an initialization failure", func() {
		registry.SetInitialized(errors.New("registration failed"))

		response := request(registry.Readyz)
		Expect(response.Code).To(Equal(http.StatusServiceUnavailable))
		Expect(response.Body.String()).To(ContainSubstring("registration failed"))
	})

	It("returns unavailable while shutting down", func() {
		cancel()
		Expect(request(registry.Healthz).Code).To(Equal(http.StatusServiceUnavailable))
		Expect(request(registry.Readyz).Code).To(Equal(http.StatusServiceUnavailable))
	})
})
