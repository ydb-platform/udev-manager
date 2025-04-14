package mux_test

import (
	"github.com/ydb-platform/udev-manager/internal/mux"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Filter", func() {
	It("should filter elements based on the provided condition", func() {
		input := make(chan int)
		go func() {
			defer close(input)
			for i := 0; i < 10; i++ {
				input <- i
			}
		}()

		evenFilter := func(n int) bool {
			return n%2 == 0
		}

		result := mux.Filter(input, evenFilter)

		var filtered []int
		for val := range result {
			filtered = append(filtered, val)
		}

		Expect(filtered).To(Equal([]int{0, 2, 4, 6, 8}))
	})

	It("should return empty channel when filter rejects all", func() {
		input := make(chan string)
		go func() {
			defer close(input)
			input <- "hello"
			input <- "world"
		}()

		noneFilter := func(s string) bool {
			return false
		}

		result := mux.Filter(input, noneFilter)

		var filtered []string
		for val := range result {
			filtered = append(filtered, val)
		}

		Expect(filtered).To(BeEmpty())
	})
})

var _ = Describe("Not", func() {
	It("should invert the filter condition", func() {
		isEven := func(n int) bool {
			return n%2 == 0
		}

		isOdd := mux.Not(isEven)

		Expect(isEven(2)).To(BeTrue())
		Expect(isOdd(2)).To(BeFalse())

		Expect(isEven(3)).To(BeFalse())
		Expect(isOdd(3)).To(BeTrue())
	})
})

var _ = Describe("Or", func() {
	It("should return true if any filter returns true", func() {
		isEven := func(n int) bool { return n%2 == 0 }
		isDivisibleBy3 := func(n int) bool { return n%3 == 0 }

		combined := mux.Or(isEven, isDivisibleBy3)

		Expect(combined(1)).To(BeFalse())
		Expect(combined(2)).To(BeTrue()) // Even
		Expect(combined(3)).To(BeTrue()) // Divisible by 3
		Expect(combined(4)).To(BeTrue()) // Even
		Expect(combined(5)).To(BeFalse())
		Expect(combined(6)).To(BeTrue()) // Both even and divisible by 3
	})

	It("should return false when no filters provided", func() {
		combined := mux.Or[int]()
		Expect(combined(42)).To(BeFalse())
	})
})

var _ = Describe("And", func() {
	It("should return true only if all filters return true", func() {
		isEven := func(n int) bool { return n%2 == 0 }
		isDivisibleBy3 := func(n int) bool { return n%3 == 0 }

		combined := mux.And(isEven, isDivisibleBy3)

		Expect(combined(1)).To(BeFalse())
		Expect(combined(2)).To(BeFalse()) // Only even
		Expect(combined(3)).To(BeFalse()) // Only divisible by 3
		Expect(combined(6)).To(BeTrue())  // Both even and divisible by 3
		Expect(combined(12)).To(BeTrue()) // Both even and divisible by 3
	})

	It("should return true when no filters provided", func() {
		combined := mux.And[int]()
		Expect(combined(42)).To(BeTrue())
	})
})

var _ = Describe("IsNil", func() {
	It("should return true for nil values", func() {
		var nilPtr *int
		var nilSlice []int
		var nilMap map[string]int
		var nilChan chan int
		var nilFunc func()
		var nilInterface interface{}

		Expect(mux.IsNil(nilPtr)).To(BeTrue())
		Expect(mux.IsNil(nilSlice)).To(BeTrue())
		Expect(mux.IsNil(nilMap)).To(BeTrue())
		Expect(mux.IsNil(nilChan)).To(BeTrue())
		Expect(mux.IsNil(nilFunc)).To(BeTrue())
		Expect(mux.IsNil(nilInterface)).To(BeTrue())
	})

	It("should return false for non-nil values", func() {
		ptr := new(int)
		slice := []int{}
		m := make(map[string]int)
		ch := make(chan int)
		fn := func() {}
		var iface interface{} = 42

		Expect(mux.IsNil(ptr)).To(BeFalse())
		Expect(mux.IsNil(slice)).To(BeFalse())
		Expect(mux.IsNil(m)).To(BeFalse())
		Expect(mux.IsNil(ch)).To(BeFalse())
		Expect(mux.IsNil(fn)).To(BeFalse())
		Expect(mux.IsNil(iface)).To(BeFalse())
	})

	It("should return false for basic types", func() {
		Expect(mux.IsNil(42)).To(BeFalse())
		Expect(mux.IsNil("hello")).To(BeFalse())
		Expect(mux.IsNil(true)).To(BeFalse())
		Expect(mux.IsNil(3.14)).To(BeFalse())
	})
})

var _ = Describe("IsNotNil", func() {
	It("should return false for nil values", func() {
		var nilPtr *int
		var nilSlice []int
		var nilInterface interface{}

		Expect(mux.IsNotNil(nilPtr)).To(BeFalse())
		Expect(mux.IsNotNil(nilSlice)).To(BeFalse())
		Expect(mux.IsNotNil(nilInterface)).To(BeFalse())
	})

	It("should return true for non-nil values", func() {
		ptr := new(int)
		slice := []int{}
		var iface interface{} = 42

		Expect(mux.IsNotNil(ptr)).To(BeTrue())
		Expect(mux.IsNotNil(slice)).To(BeTrue())
		Expect(mux.IsNotNil(iface)).To(BeTrue())
		Expect(mux.IsNotNil(42)).To(BeTrue())
		Expect(mux.IsNotNil("hello")).To(BeTrue())
	})
})
